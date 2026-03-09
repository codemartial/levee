package backend

import (
	"container/heap"
	"math"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/loadgen"
)

// Server simulates a capacity-limited backend service with load-dependent degradation.
// Uses logical time from request timestamps — no wall clock sleeping.
//
// The server is authoritative about request completion timing. Callers submit
// requests via Submit(), and completions are delivered via callback during
// Advance() when logical time reaches the completion point.
type Server struct {
	capacity *CapacityController
	pool     *WorkerPool
	latency  *LatencyGenerator
	specs    []loadgen.LoadSpec

	slotsPerReplica int

	// Logical time tracking (max timestamp seen)
	logicalTimeNS atomic.Int64

	// Completion heap — requests awaiting delivery to the caller.
	// Ordered by completionNS (earliest first). Only accessed from a
	// single event loop goroutine, but uses heap.Interface for ordering.
	completions completionHeap

	// Metrics
	totalRequests   atomic.Int64
	totalSuccesses  atomic.Int64
	totalFailures   atomic.Int64
	totalShed       atomic.Int64
	totalQueueDrops atomic.Int64
	totalCrashes    atomic.Int64
}

// completionEntry represents a pending request completion on the backend.
type completionEntry struct {
	completionNS int64
	latency      time.Duration
	success      bool
	tag          int
}

// completionHeap is a min-heap of completionEntry ordered by completionNS.
type completionHeap []completionEntry

func (h completionHeap) Len() int            { return len(h) }
func (h completionHeap) Less(i, j int) bool  { return h[i].completionNS < h[j].completionNS }
func (h completionHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *completionHeap) Push(x any)         { *h = append(*h, x.(completionEntry)) }
func (h *completionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// NewServer creates a new backend server.
func NewServer(specs []loadgen.LoadSpec, capacityConfig CapacityControllerConfig, seed uint64) *Server {
	capacity := NewCapacityController(capacityConfig)
	latency := NewLatencyGenerator(specs, seed)

	// Compute slots per replica using Little's Law:
	// concurrency = throughput × mean_latency
	meanLatencyMS := latency.GetProfile(0).MeanLatencyMS()
	meanLatencyS := meanLatencyMS / 1000.0
	slotsPerReplica := int(math.Ceil(float64(capacityConfig.BaseShape.ThroughputRPS) * meanLatencyS))
	if slotsPerReplica < 1 {
		slotsPerReplica = 1
	}

	pool := NewWorkerPool(
		slotsPerReplica,
		capacity.CurrentReplicas(),
		capacity.CurrentQueueDepth(),
	)

	return &Server{
		capacity:        capacity,
		pool:            pool,
		latency:         latency,
		specs:           specs,
		slotsPerReplica: slotsPerReplica,
		completions:     make(completionHeap, 0, 256),
	}
}

// Stats returns backend processing statistics.
type ServerStats struct {
	TotalRequests   int64
	TotalSuccesses  int64
	TotalFailures   int64
	TotalShed       int64
	TotalQueueDrops int64
	TotalCrashes    int64
}

func (s *Server) Stats() ServerStats {
	return ServerStats{
		TotalRequests:   s.totalRequests.Load(),
		TotalSuccesses:  s.totalSuccesses.Load(),
		TotalFailures:   s.totalFailures.Load(),
		TotalShed:       s.totalShed.Load(),
		TotalQueueDrops: s.totalQueueDrops.Load(),
		TotalCrashes:    s.totalCrashes.Load(),
	}
}

// ServerStatus provides real-time backend status for tracing.
type ServerStatus struct {
	Replicas   int
	EWMALoad   float64
	CurrentRPS float64
}

// Status returns the current real-time backend status.
func (s *Server) Status() ServerStatus {
	return ServerStatus{
		Replicas:   s.capacity.CurrentReplicas(),
		EWMALoad:   s.pool.EWMALoad(),
		CurrentRPS: s.capacity.GetCurrentRPS(),
	}
}

// Advance advances the backend's logical clock, ticks the HPA, and delivers
// completions whose logical time has elapsed via the onComplete callback.
//
// Called on every request (both allowed and blocked by the CB). When the CB
// blocks traffic, this ensures the autoscaler still ticks and pending
// completions are delivered so the CB can update its state.
//
// The onComplete callback is invoked synchronously and must not call back
// into Server methods.
func (s *Server) Advance(timestampNS int64, onComplete func(api.Completion)) {
	// Update logical time to max of current and request timestamp
	for {
		current := s.logicalTimeNS.Load()
		if timestampNS <= current {
			break
		}
		if s.logicalTimeNS.CompareAndSwap(current, timestampNS) {
			break
		}
	}

	// During crash, HPA is frozen — only attempt recovery
	if s.pool.IsCrashed() {
		if s.pool.TryRecover(timestampNS) {
			s.totalCrashes.Add(1)
			s.capacity.ResetAfterCrash()
			s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())
		}
	} else {
		logicalNow := time.Unix(0, timestampNS)
		s.capacity.Tick(logicalNow)
		s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())
	}

	// Deliver completions whose logical time has elapsed
	for s.completions.Len() > 0 && s.completions[0].completionNS <= timestampNS {
		entry := heap.Pop(&s.completions).(completionEntry)
		onComplete(api.Completion{
			CompletionNS: entry.completionNS,
			Latency:      entry.latency,
			Success:      entry.success,
			Tag:          entry.tag,
		})
	}
}

// Submit processes a backend request and schedules a completion on the
// internal heap. The completion will be delivered via the onComplete callback
// during a future Advance() call when logical time reaches the completion point.
//
// Only called for requests that the circuit breaker allowed through.
func (s *Server) Submit(req api.BackendRequest) {
	s.totalRequests.Add(1)

	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	timeoutNS := timeout.Nanoseconds()

	// Check crash state — HPA is frozen during crash
	if s.pool.IsCrashed() {
		if !s.pool.TryRecover(req.TimestampNS) {
			// Still crashed — schedule failure at timeout
			s.totalFailures.Add(1)
			heap.Push(&s.completions, completionEntry{
				completionNS: req.TimestampNS + timeoutNS,
				latency:      timeout,
				success:      false,
				tag:          req.Tag,
			})
			return
		}
		// Just recovered — reset HPA capacity and resize pool
		s.totalCrashes.Add(1)
		s.capacity.ResetAfterCrash()
		s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())
	}

	logicalNow := time.Unix(0, req.TimestampNS)

	// Update capacity controller with logical time
	s.capacity.RecordRequest(logicalNow)
	s.capacity.Tick(logicalNow)

	// Reconcile worker pool with current HPA state
	s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())

	// Sample nominal latency from spec profile (healthyLatencyLookup applied internally)
	latencyMS := s.latency.Sample(req.SpecIndex)
	nominalProcessingNS := int64(latencyMS * float64(time.Millisecond))

	// Process through the worker pool (applies degradation)
	result := s.pool.TryProcess(req.TimestampNS, nominalProcessingNS, timeoutNS)

	switch result.Status {
	case ProcessCrashed:
		// Crash just triggered — fail all pending completions
		s.convertPendingToFailures(timeoutNS)
		s.totalFailures.Add(1)
		heap.Push(&s.completions, completionEntry{
			completionNS: req.TimestampNS + timeoutNS,
			latency:      timeout,
			success:      false,
			tag:          req.Tag,
		})

	case ProcessShed:
		s.totalShed.Add(1)
		s.totalFailures.Add(1)
		heap.Push(&s.completions, completionEntry{
			completionNS: req.TimestampNS + timeoutNS,
			latency:      timeout,
			success:      false,
			tag:          req.Tag,
		})

	case ProcessTimeout:
		s.totalQueueDrops.Add(1)
		s.totalFailures.Add(1)
		heap.Push(&s.completions, completionEntry{
			completionNS: req.TimestampNS + timeoutNS,
			latency:      timeout,
			success:      false,
			tag:          req.Tag,
		})

	case ProcessOK:
		// Compute total latency: queue wait + degraded processing time
		scaledProcessingNS := int64(float64(nominalProcessingNS) * result.LatencyScale)
		totalLatencyNS := result.QueueWait + scaledProcessingNS

		success := true

		// Check load-driven errors (from degradation curve)
		if result.LoadErrorRate > 0 && s.latency.RollFloat64() < result.LoadErrorRate {
			success = false
		}

		// Check baseline errors (independent of load, follows BAU degradation)
		if success && s.latency.ShouldError(req.SpecIndex) {
			success = false
		}

		// Check if degraded latency exceeds timeout — client would have given up
		if totalLatencyNS >= timeoutNS {
			success = false
			totalLatencyNS = timeoutNS
		}

		if success {
			s.totalSuccesses.Add(1)
		} else {
			s.totalFailures.Add(1)
		}

		heap.Push(&s.completions, completionEntry{
			completionNS: req.TimestampNS + totalLatencyNS,
			latency:      time.Duration(totalLatencyNS),
			success:      success,
			tag:          req.Tag,
		})
	}
}

// convertPendingToFailures converts all pending successful completions to
// failures. Called when a crash triggers to fail in-flight requests that
// were optimistically scheduled as successes.
func (s *Server) convertPendingToFailures(timeoutNS int64) {
	for i := range s.completions {
		if s.completions[i].success {
			s.completions[i].success = false
			s.completions[i].latency = time.Duration(timeoutNS)
			// completionNS unchanged — ordering key preserved, no re-heapify needed
		}
	}
}
