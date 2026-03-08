// Package app implements the application container for the distributed benchmark.
package app

import (
	"container/heap"
	"math"
	"sync"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/loadgen"
)

// CircuitBreaker is the interface that all CBs must implement.
type CircuitBreaker interface {
	Start(time.Time) (levee.StateChange, error)
	Success(time.Time, time.Duration) levee.StateChange
	Fail(time.Time, time.Duration) levee.StateChange
	State() levee.State
}

// cbRequest is sent from callers to per-CB event loops.
type cbRequest struct {
	req    api.AppRequest
	ts     time.Time
	result chan<- api.CBResult
}

// pendingCompletion represents a backend call that has completed but hasn't been
// reported to the CB yet. Completions are processed in logical-time order.
type pendingCompletion struct {
	completionNS int64
	latency      time.Duration
	success      bool
}

// completionHeap is a min-heap of pendingCompletion ordered by completionNS.
type completionHeap []pendingCompletion

func (h completionHeap) Len() int            { return len(h) }
func (h completionHeap) Less(i, j int) bool  { return h[i].completionNS < h[j].completionNS }
func (h completionHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *completionHeap) Push(x any)         { *h = append(*h, x.(pendingCompletion)) }
func (h *completionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// CBEntry holds a circuit breaker and its name.
type CBEntry struct {
	Name     string
	CB       CircuitBreaker
	Metrics  *CBMetrics
	requests chan cbRequest
}

// CBMetrics tracks per-CB metrics.
type CBMetrics struct {
	mu               sync.RWMutex
	TotalAllowed     int64
	TotalBlocked     int64
	TotalSuccesses   int64
	TotalFailures    int64
	StateTransitions int
	LastState        levee.State

	// Concurrency tracking (only while NOT OPEN)
	currentConcurrency int64 // currently in-flight requests for this CB
	maxConcurrency     int64 // max observed while state != OPEN

	// Epoch-based scoring (200ms = 200_000_000 ns)
	currentEpoch    int64   // timestampNS / 200_000_000
	epochSuccesses  int64   // count in current epoch
	epochFailures   int64   // count in current epoch
	successScoreSum float64 // running sum of (num_s² × 5)
	failureScoreSum float64 // running sum of (num_f² × 5)
}

// Orchestrator manages all circuit breakers and routes requests.
type Orchestrator struct {
	mu            sync.RWMutex
	cbs           []CBEntry
	callBackendFn func(api.BackendRequest) api.BackendResponse
	advanceTimeFn func(int64)
	specs         []loadgen.LoadSpec
	slo           levee.SLO
	startTime     time.Time
	totalRequests int64

	// Logical time tracking (max timestamp seen)
	maxLogicalTimeNS int64

	// Event loop lifecycle
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// RecordResult updates epoch-based scoring for a result.
func (m *CBMetrics) RecordResult(timestampNS int64, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	epoch := timestampNS / 200_000_000 // 200ms epochs

	if epoch != m.currentEpoch {
		// Finalize previous epoch before moving to new one
		m.successScoreSum += float64(m.epochSuccesses*m.epochSuccesses) * 5
		m.failureScoreSum += float64(m.epochFailures*m.epochFailures) * 5
		m.epochSuccesses = 0
		m.epochFailures = 0
		m.currentEpoch = epoch
	}

	if success {
		m.epochSuccesses++
	} else {
		m.epochFailures++
	}
}

// Finalize flushes the last epoch's scores.
func (m *CBMetrics) Finalize() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Flush the last epoch
	m.successScoreSum += float64(m.epochSuccesses*m.epochSuccesses) * 5
	m.failureScoreSum += float64(m.epochFailures*m.epochFailures) * 5
	m.epochSuccesses = 0
	m.epochFailures = 0
}

// OrchestratorConfig holds configuration for the orchestrator.
type OrchestratorConfig struct {
	CallBackend func(api.BackendRequest) api.BackendResponse
	AdvanceTime func(int64) // Advances backend logical clock without recording demand
	SLO         levee.SLO
	Specs       []loadgen.LoadSpec
	StartTime   time.Time
	CBName      string // If set, only run this CB (for isolated testing)
}

// NewOrchestrator creates a new orchestrator.
// If CBName is set, only that CB is tested (for isolated benchmarking).
// If CBName is empty, all 3 CBs are tested (for comparison, but with shared backend).
func NewOrchestrator(config OrchestratorConfig) *Orchestrator {
	o := &Orchestrator{
		callBackendFn: config.CallBackend,
		advanceTimeFn: config.AdvanceTime,
		specs:         config.Specs,
		slo:           config.SLO,
		startTime:     config.StartTime,
	}

	// Initialize circuit breakers based on config
	allCBs := []CBEntry{
		{
			Name:    "No-CB",
			CB:      benchmarks.NewNoCB(),
			Metrics: &CBMetrics{LastState: levee.CLOSED},
		},
		{
			Name:    "Levee",
			CB:      levee.NewLevee(config.SLO),
			Metrics: &CBMetrics{LastState: levee.CLOSED},
		},
		{
			Name:    "Static-BAU",
			CB:      benchmarks.NewStaticBAU(),
			Metrics: &CBMetrics{LastState: levee.CLOSED},
		},
		{
			Name:    "Static-Peak",
			CB:      benchmarks.NewStaticPeak(),
			Metrics: &CBMetrics{LastState: levee.CLOSED},
		},
	}

	if config.CBName != "" {
		// Single CB mode - only test the specified CB
		for _, cb := range allCBs {
			if cb.Name == config.CBName {
				o.cbs = []CBEntry{cb}
				break
			}
		}
		if len(o.cbs) == 0 {
			panic("unknown CB name: " + config.CBName)
		}
	} else {
		// All CBs mode (warning: they share backend capacity)
		o.cbs = allCBs
	}

	// Start per-CB event loops
	for i := range o.cbs {
		o.cbs[i].requests = make(chan cbRequest, 64)
		o.wg.Add(1)
		go func(entry *CBEntry) {
			defer o.wg.Done()
			o.cbEventLoop(entry)
		}(&o.cbs[i])
	}

	return o
}

// HandleRequest processes a request through all circuit breakers.
func (o *Orchestrator) HandleRequest(req api.AppRequest) api.AppResponse {
	o.mu.Lock()
	o.totalRequests++
	// Track max logical time for metrics
	if req.TimestampNS > o.maxLogicalTimeNS {
		o.maxLogicalTimeNS = req.TimestampNS
	}
	o.mu.Unlock()

	ts := time.Unix(0, req.TimestampNS)

	// Send to all CB event loops and collect results
	resultChs := make([]chan api.CBResult, len(o.cbs))
	for i := range o.cbs {
		ch := make(chan api.CBResult, 1)
		resultChs[i] = ch
		o.cbs[i].requests <- cbRequest{req: req, ts: ts, result: ch}
	}

	results := make(map[string]api.CBResult)
	for i := range o.cbs {
		results[o.cbs[i].Name] = <-resultChs[i]
	}

	return api.AppResponse{
		RequestID: req.RequestID,
		Results:   results,
	}
}

// cbEventLoop processes CB lifecycle events in logical-time order.
// Each CB has its own event loop goroutine. A min-heap of pending completions
// ensures Success()/Fail() are called in logical completion time order.
func (o *Orchestrator) cbEventLoop(entry *CBEntry) {
	var h completionHeap
	var concurrency int64
	var maxConcurrency int64

	drainCompletion := func(c pendingCompletion) {
		completionTime := time.Unix(0, c.completionNS)
		if c.success {
			entry.CB.Success(completionTime, c.latency)
		} else {
			entry.CB.Fail(completionTime, c.latency)
		}
		concurrency--

		entry.Metrics.mu.Lock()
		if c.success {
			entry.Metrics.TotalSuccesses++
		} else {
			entry.Metrics.TotalFailures++
		}
		entry.Metrics.currentConcurrency = concurrency
		entry.Metrics.mu.Unlock()

		entry.Metrics.RecordResult(c.completionNS, c.success)
	}

	for req := range entry.requests {
		// Advance backend's logical clock so HPA can tick even when CB blocks traffic.
		// This simulates the real-world behavior where the autoscaler runs on its own
		// clock, independent of whether traffic reaches the backend.
		if o.advanceTimeFn != nil {
			o.advanceTimeFn(req.req.TimestampNS)
		}

		// Drain completions that logically finished before this request's start
		for h.Len() > 0 && h[0].completionNS <= req.req.TimestampNS {
			drainCompletion(heap.Pop(&h).(pendingCompletion))
		}

		// Process the new request
		sc, err := entry.CB.Start(req.ts)

		// Track state transitions
		entry.Metrics.mu.Lock()
		if sc.State != entry.Metrics.LastState {
			entry.Metrics.StateTransitions++
			entry.Metrics.LastState = sc.State
		}
		entry.Metrics.mu.Unlock()

		if err != nil {
			// CB blocked the request
			entry.Metrics.mu.Lock()
			entry.Metrics.TotalBlocked++
			entry.Metrics.mu.Unlock()

			req.result <- api.CBResult{
				Allowed:   false,
				State:     stateString(sc.State),
				LatencyUS: 0,
				Success:   false,
				Error:     "circuit open",
			}
			continue
		}

		// CB allowed - track concurrency
		concurrency++
		entry.Metrics.mu.Lock()
		entry.Metrics.TotalAllowed++
		entry.Metrics.currentConcurrency = concurrency
		if sc.State != levee.OPEN && concurrency > maxConcurrency {
			maxConcurrency = concurrency
			entry.Metrics.maxConcurrency = maxConcurrency
		}
		entry.Metrics.mu.Unlock()

		// Direct backend call
		backendResp := o.callBackend(req.req, entry.Name)

		// Queue completion for logical-time-ordered processing
		latency := time.Duration(backendResp.LatencyUS) * time.Microsecond
		completionNS := req.req.TimestampNS + latency.Nanoseconds()
		heap.Push(&h, pendingCompletion{
			completionNS: completionNS,
			latency:      latency,
			success:      backendResp.Success,
		})

		// Build and send result back to caller
		var resultErr string
		if !backendResp.Success {
			resultErr = backendResp.Error
		}

		req.result <- api.CBResult{
			Allowed:   true,
			State:     stateString(entry.CB.State()),
			LatencyUS: backendResp.LatencyUS,
			Success:   backendResp.Success,
			Error:     resultErr,
		}
	}

	// Drain remaining completions after channel closes
	for h.Len() > 0 {
		drainCompletion(heap.Pop(&h).(pendingCompletion))
	}

	// Finalize epoch scoring
	entry.Metrics.Finalize()
}

// Close shuts down all per-CB event loops and drains remaining completions.
func (o *Orchestrator) Close() {
	o.closeOnce.Do(func() {
		for i := range o.cbs {
			close(o.cbs[i].requests)
		}
		o.wg.Wait()
	})
}

// callBackend makes a direct call to the backend.
func (o *Orchestrator) callBackend(req api.AppRequest, cbName string) api.BackendResponse {
	return o.callBackendFn(api.BackendRequest{
		RequestID:   req.RequestID,
		CBName:      cbName,
		TimestampNS: req.TimestampNS,
		TimeoutMS:   req.TimeoutMS,
		SpecIndex:   req.SpecIndex,
	})
}

// GetMetrics returns the current metrics for all CBs.
func (o *Orchestrator) GetMetrics() api.AppMetrics {
	// Drain event loops before reporting final metrics
	o.Close()

	o.mu.RLock()
	totalRequests := o.totalRequests
	maxLogicalTimeNS := o.maxLogicalTimeNS
	o.mu.RUnlock()

	cbMetrics := make(map[string]api.CBMetrics)
	for _, entry := range o.cbs {
		// Finalize epoch scoring before reading
		entry.Metrics.Finalize()

		entry.Metrics.mu.RLock()
		cbMetrics[entry.Name] = api.CBMetrics{
			State:            stateString(entry.CB.State()),
			TotalAllowed:     entry.Metrics.TotalAllowed,
			TotalBlocked:     entry.Metrics.TotalBlocked,
			TotalSuccesses:   entry.Metrics.TotalSuccesses,
			TotalFailures:    entry.Metrics.TotalFailures,
			StateTransitions: entry.Metrics.StateTransitions,
			SuccessScore:     math.Sqrt(entry.Metrics.successScoreSum),
			FailureScore:     math.Sqrt(entry.Metrics.failureScoreSum),
			MaxConcurrency:   entry.Metrics.maxConcurrency,
		}
		entry.Metrics.mu.RUnlock()
	}

	// Use logical elapsed time (from timestamp 0)
	logicalElapsedSeconds := float64(maxLogicalTimeNS) / float64(time.Second)

	return api.AppMetrics{
		CircuitBreakers: cbMetrics,
		TotalRequests:   totalRequests,
		ElapsedSeconds:  logicalElapsedSeconds,
	}
}

// GetCBs returns the CB entries for checkpointing.
func (o *Orchestrator) GetCBs() []CBEntry {
	return o.cbs
}

// stateString converts levee.State to a string.
func stateString(s levee.State) string {
	switch s {
	case levee.CLOSED:
		return "CLOSED"
	case levee.OPEN:
		return "OPEN"
	case levee.THROTTLED:
		return "THROTTLED"
	default:
		return "UNKNOWN"
	}
}
