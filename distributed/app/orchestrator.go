// Package app implements the application container for the distributed benchmark.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/distributed/api"
	"github.com/codemartial/loadgen"
)

// CircuitBreaker is the interface that all CBs must implement.
type CircuitBreaker interface {
	Start(time.Time) (levee.StateChange, error)
	Success(time.Time, time.Duration) levee.StateChange
	Fail(time.Time, time.Duration) levee.StateChange
	State() levee.State
}

// CBEntry holds a circuit breaker and its name.
type CBEntry struct {
	Name    string
	CB      CircuitBreaker
	Metrics *CBMetrics
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
	backendHost   string
	httpClient    *http.Client
	specs         []loadgen.LoadSpec
	slo           levee.SLO
	startTime     time.Time
	totalRequests int64

	// Logical time tracking (max timestamp seen)
	maxLogicalTimeNS int64
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
	BackendHost string
	SLO         levee.SLO
	Specs       []loadgen.LoadSpec
	StartTime   time.Time
	CBName      string // If set, only run this CB (for isolated testing)
}

// NewOrchestrator creates a new orchestrator.
// If CBName is set, only that CB is tested (for isolated benchmarking).
// If CBName is empty, all 3 CBs are tested (for comparison, but with shared backend).
func NewOrchestrator(config OrchestratorConfig) *Orchestrator {
	// Create HTTP transport - use Unix socket if backend host starts with "/"
	var transport *http.Transport
	if len(config.BackendHost) > 0 && config.BackendHost[0] == '/' {
		// Unix socket transport
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", config.BackendHost)
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
	} else {
		// TCP transport
		transport = &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
	}

	o := &Orchestrator{
		backendHost: config.BackendHost,
		specs:       config.Specs,
		slo:         config.SLO,
		startTime:   config.StartTime,
		httpClient: &http.Client{
			Timeout:   time.Duration(config.SLO.Timeout.Milliseconds()*2) * time.Millisecond,
			Transport: transport,
		},
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

	return o
}

// HandleRequest processes a request through all circuit breakers.
func (o *Orchestrator) HandleRequest(ctx context.Context, req api.AppRequest) api.AppResponse {
	o.mu.Lock()
	o.totalRequests++
	// Track max logical time for metrics
	if req.TimestampNS > o.maxLogicalTimeNS {
		o.maxLogicalTimeNS = req.TimestampNS
	}
	o.mu.Unlock()

	ts := time.Unix(0, req.TimestampNS)

	results := make(map[string]api.CBResult)

	// Process each CB
	for i := range o.cbs {
		entry := &o.cbs[i]
		result := o.processCB(ctx, entry, req, ts)
		results[entry.Name] = result
	}

	return api.AppResponse{
		RequestID: req.RequestID,
		Results:   results,
	}
}

// processCB handles a single CB's processing of a request.
func (o *Orchestrator) processCB(ctx context.Context, entry *CBEntry, req api.AppRequest, ts time.Time) api.CBResult {
	// Call CB.Start
	sc, err := entry.CB.Start(ts)

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

		return api.CBResult{
			Allowed:   false,
			State:     stateString(sc.State),
			LatencyUS: 0,
			Success:   false,
			Error:     "circuit open",
		}
	}

	// CB allowed - make backend call
	entry.Metrics.mu.Lock()
	entry.Metrics.TotalAllowed++
	entry.Metrics.mu.Unlock()

	backendResp, latency, backendErr := o.callBackend(ctx, req, entry.Name)

	var success bool
	var resultErr string

	if backendErr != nil {
		success = false
		resultErr = backendErr.Error()
		entry.CB.Fail(ts.Add(latency), latency)
		entry.Metrics.mu.Lock()
		entry.Metrics.TotalFailures++
		entry.Metrics.mu.Unlock()
	} else if backendResp != nil && !backendResp.Success {
		success = false
		resultErr = backendResp.Error
		entry.CB.Fail(ts.Add(latency), latency)
		entry.Metrics.mu.Lock()
		entry.Metrics.TotalFailures++
		entry.Metrics.mu.Unlock()
	} else {
		success = true
		entry.CB.Success(ts.Add(latency), latency)
		entry.Metrics.mu.Lock()
		entry.Metrics.TotalSuccesses++
		entry.Metrics.mu.Unlock()
	}

	// Record result for epoch-based scoring
	entry.Metrics.RecordResult(req.TimestampNS, success)

	var latencyUS int64
	if backendResp != nil {
		latencyUS = backendResp.LatencyUS
	} else {
		latencyUS = latency.Microseconds()
	}

	return api.CBResult{
		Allowed:   true,
		State:     stateString(entry.CB.State()),
		LatencyUS: latencyUS,
		Success:   success,
		Error:     resultErr,
	}
}

// callBackend makes an HTTP call to the backend.
// Returns the logical latency from the backend response (not wall clock).
func (o *Orchestrator) callBackend(ctx context.Context, req api.AppRequest, cbName string) (*api.BackendResponse, time.Duration, error) {
	backendReq := api.BackendRequest{
		RequestID:   req.RequestID,
		CBName:      cbName,
		TimestampNS: req.TimestampNS,
		TimeoutMS:   req.TimeoutMS,
		SpecIndex:   req.SpecIndex,
	}

	body, err := json.Marshal(backendReq)
	if err != nil {
		// On error, return timeout as the latency (logical)
		return nil, time.Duration(req.TimeoutMS) * time.Millisecond, fmt.Errorf("marshal error: %w", err)
	}

	// For Unix sockets, use a dummy host since the transport handles the connection
	var url string
	if len(o.backendHost) > 0 && o.backendHost[0] == '/' {
		url = "http://unix/execute"
	} else {
		url = fmt.Sprintf("http://%s/execute", o.backendHost)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, time.Duration(req.TimeoutMS) * time.Millisecond, fmt.Errorf("request creation error: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(httpReq)
	if err != nil {
		// On HTTP error, assume timeout (logical)
		return nil, time.Duration(req.TimeoutMS) * time.Millisecond, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	var backendResp api.BackendResponse
	if err := json.NewDecoder(resp.Body).Decode(&backendResp); err != nil {
		return nil, time.Duration(req.TimeoutMS) * time.Millisecond, fmt.Errorf("decode error: %w", err)
	}

	// Use the logical latency from the backend response
	logicalLatency := time.Duration(backendResp.LatencyUS) * time.Microsecond

	return &backendResp, logicalLatency, nil
}

// GetMetrics returns the current metrics for all CBs.
func (o *Orchestrator) GetMetrics() api.AppMetrics {
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
