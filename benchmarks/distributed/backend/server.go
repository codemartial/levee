package backend

import (
	"math"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/loadgen"
)

// Server simulates a capacity-limited backend service with load-dependent degradation.
// Uses logical time from request timestamps — no wall clock sleeping.
type Server struct {
	capacity *CapacityController
	pool     *WorkerPool
	latency  *LatencyGenerator
	specs    []loadgen.LoadSpec

	slotsPerReplica int

	// Logical time tracking (max timestamp seen)
	logicalTimeNS atomic.Int64

	// Metrics
	totalRequests   atomic.Int64
	totalSuccesses  atomic.Int64
	totalFailures   atomic.Int64
	totalShed       atomic.Int64
	totalQueueDrops atomic.Int64
	totalCrashes    atomic.Int64
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

// AdvanceTime advances the backend's logical clock and ticks the HPA without
// recording any demand. This allows the autoscaler to evaluate scaling decisions
// and promote pending replicas even when a circuit breaker is blocking traffic.
func (s *Server) AdvanceTime(timestampNS int64) {
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
		return
	}

	logicalNow := time.Unix(0, timestampNS)
	s.capacity.Tick(logicalNow)
	s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())
}

// Execute processes a backend request using logical time and returns the result.
// Load-dependent degradation inflates latencies and escalates errors when the
// backend is overloaded. Sustained extreme overload triggers node crash simulation.
func (s *Server) Execute(req api.BackendRequest) api.BackendResponse {
	s.totalRequests.Add(1)

	// Update logical time to max of current and request timestamp
	for {
		current := s.logicalTimeNS.Load()
		if req.TimestampNS <= current {
			break
		}
		if s.logicalTimeNS.CompareAndSwap(current, req.TimestampNS) {
			break
		}
	}

	timeout := time.Duration(req.TimeoutMS) * time.Millisecond

	// Check crash state — HPA is frozen during crash
	if s.pool.IsCrashed() {
		if !s.pool.TryRecover(req.TimestampNS) {
			// Still crashed — don't record request or tick HPA
			s.totalFailures.Add(1)
			return api.BackendResponse{
				RequestID: req.RequestID,
				LatencyUS: timeout.Microseconds(),
				Success:   false,
				Error:     "service unavailable: node crashed",
			}
		}
		// Just recovered — reset HPA capacity and resize pool
		s.totalCrashes.Add(1)
		s.capacity.ResetAfterCrash()
		s.pool.Resize(s.capacity.CurrentReplicas(), s.capacity.CurrentQueueDepth())
	}

	logicalNow := time.Unix(0, req.TimestampNS)
	timeoutNS := timeout.Nanoseconds()

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
		s.totalFailures.Add(1)
		return api.BackendResponse{
			RequestID: req.RequestID,
			LatencyUS: timeout.Microseconds(),
			Success:   false,
			Queued:    false,
			Shed:      false,
			Error:     "service unavailable: node crashed",
		}

	case ProcessShed:
		s.totalShed.Add(1)
		s.totalFailures.Add(1)
		return api.BackendResponse{
			RequestID: req.RequestID,
			LatencyUS: timeout.Microseconds(),
			Success:   false,
			Queued:    false,
			Shed:      true,
			Error:     "service unavailable: queue full",
		}

	case ProcessTimeout:
		s.totalQueueDrops.Add(1)
		s.totalFailures.Add(1)
		return api.BackendResponse{
			RequestID: req.RequestID,
			LatencyUS: timeout.Microseconds(),
			Success:   false,
			Queued:    result.QueueWait > 0,
			Shed:      false,
			Error:     "queue timeout",
		}

	case ProcessOK:
		// Compute total latency: queue wait + degraded processing time
		scaledProcessingNS := int64(float64(nominalProcessingNS) * result.LatencyScale)
		totalLatencyNS := result.QueueWait + scaledProcessingNS
		totalLatencyUS := totalLatencyNS / 1000

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
			totalLatencyUS = timeout.Microseconds()
		}

		if success {
			s.totalSuccesses.Add(1)
		} else {
			s.totalFailures.Add(1)
		}

		return api.BackendResponse{
			RequestID: req.RequestID,
			LatencyUS: totalLatencyUS,
			Success:   success,
			Queued:    result.QueueWait > 0,
			Shed:      false,
		}
	}

	// Unreachable
	s.totalFailures.Add(1)
	return api.BackendResponse{RequestID: req.RequestID, Success: false, Error: "unknown status"}
}
