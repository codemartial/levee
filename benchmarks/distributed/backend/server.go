package backend

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/loadgen"
)

// Server is the backend HTTP server that simulates a capacity-limited service.
// Uses logical time from request timestamps - no wall clock sleeping.
type Server struct {
	capacity *CapacityController
	queue    *RequestQueue
	latency  *LatencyGenerator
	specs    []loadgen.LoadSpec

	mu sync.Mutex

	// Logical time tracking (max timestamp seen)
	logicalTimeNS atomic.Int64

	// Metrics
	totalRequests   atomic.Int64
	totalSuccesses  atomic.Int64
	totalFailures   atomic.Int64
	totalShed       atomic.Int64
	totalQueueDrops atomic.Int64

	// Wall time tracking (for throughput reporting)
	wallStartTime time.Time
}

// NewServer creates a new backend server.
func NewServer(specs []loadgen.LoadSpec, capacityConfig CapacityControllerConfig, seed uint64) *Server {
	capacity := NewCapacityController(capacityConfig)
	queue := NewRequestQueue(capacity.CurrentQueueDepth())
	latency := NewLatencyGenerator(specs, seed)

	return &Server{
		capacity:      capacity,
		queue:         queue,
		latency:       latency,
		specs:         specs,
		wallStartTime: time.Now(),
	}
}

// HandleExecute handles POST /execute requests using logical time.
func (s *Server) HandleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req api.BackendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

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

	logicalNow := time.Unix(0, req.TimestampNS)
	timeout := time.Duration(req.TimeoutMS) * time.Millisecond

	// Update capacity controller with logical time
	s.capacity.RecordRequest(logicalNow)
	s.capacity.Tick(logicalNow)

	// Update queue max depth based on current capacity
	s.queue.UpdateMaxDepth(s.capacity.CurrentQueueDepth())

	// Check current load vs capacity
	capacity := s.capacity.CurrentCapacity()
	currentRPS := s.capacity.GetCurrentRPS()

	// Generate processing latency (no sleeping - just calculate)
	latencyMS := s.latency.Sample(req.SpecIndex)
	processingTime := time.Duration(latencyMS * float64(time.Millisecond))

	s.mu.Lock()
	defer s.mu.Unlock()

	if currentRPS >= float64(capacity) {
		// Over capacity - try to queue
		queueResult := s.queue.TryQueue(logicalNow, timeout, processingTime)

		switch queueResult.Status {
		case QueueStatusShed:
			// Queue full - immediate 503
			s.totalShed.Add(1)
			resp := api.BackendResponse{
				RequestID: req.RequestID,
				LatencyUS: 0,
				Success:   false,
				Queued:    false,
				Shed:      true,
				Error:     "service unavailable: queue full",
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(resp)
			return

		case QueueStatusTimeout:
			// Would timeout in queue - 504
			s.totalQueueDrops.Add(1)
			s.totalFailures.Add(1)
			resp := api.BackendResponse{
				RequestID: req.RequestID,
				LatencyUS: timeout.Microseconds(),
				Success:   false,
				Queued:    true,
				Shed:      false,
				Error:     "queue timeout",
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGatewayTimeout)
			json.NewEncoder(w).Encode(resp)
			return

		case QueueStatusProcessed:
			// Successfully queued and processed
			totalLatency := queueResult.QueueWait + processingTime
			success := totalLatency < timeout

			// Inject baseline errors independent of capacity/latency
			if success && s.latency.ShouldError(req.SpecIndex) {
				success = false
			}

			if success {
				s.totalSuccesses.Add(1)
			} else {
				s.totalFailures.Add(1)
			}

			resp := api.BackendResponse{
				RequestID: req.RequestID,
				LatencyUS: totalLatency.Microseconds(),
				Success:   success,
				Queued:    true,
				Shed:      false,
			}
			w.Header().Set("Content-Type", "application/json")
			if success {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusGatewayTimeout)
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
	}

	// Under capacity - process immediately
	success := processingTime < timeout

	// Inject baseline errors independent of capacity/latency
	// This simulates the inherent error rate of the backend service
	if success && s.latency.ShouldError(req.SpecIndex) {
		success = false
	}

	if success {
		s.totalSuccesses.Add(1)
	} else {
		s.totalFailures.Add(1)
	}

	resp := api.BackendResponse{
		RequestID: req.RequestID,
		LatencyUS: processingTime.Microseconds(),
		Success:   success,
		Queued:    false,
		Shed:      false,
	}

	w.Header().Set("Content-Type", "application/json")
	if success {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusGatewayTimeout)
	}
	json.NewEncoder(w).Encode(resp)
}

// HandleStatus handles GET /status requests.
func (s *Server) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.capacity.Status()

	resp := api.BackendStatus{
		CurrentReplicas: status.CurrentReplicas,
		PendingReplicas: status.PendingReplicas,
		CapacityRPS:     status.CapacityRPS,
		QueueDepth:      s.queue.Len(),
		QueueMax:        s.queue.MaxDepth(),
		CurrentRPS:      status.CurrentRPS,
		SpecIndex:       0, // Not tracked in logical time mode
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Handler returns an http.Handler for the backend server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/execute", s.HandleExecute)
	mux.HandleFunc("/status", s.HandleStatus)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	return mux
}

// Run starts the HTTP server. If addr starts with "/", it's treated as a Unix socket path.
func (s *Server) Run(addr string) error {
	srv := &http.Server{
		Handler: s.Handler(),
	}

	// Check if this is a Unix socket path
	if len(addr) > 0 && addr[0] == '/' {
		return s.runUnix(srv, addr)
	}

	// TCP listener
	srv.Addr = addr
	log.Printf("Backend server starting on %s (logical time mode, TCP)", addr)
	return srv.ListenAndServe()
}

// runUnix starts the server on a Unix socket.
func (s *Server) runUnix(srv *http.Server, socketPath string) error {
	// Remove existing socket file
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket: %w", err)
	}

	log.Printf("Backend server starting on %s (logical time mode, Unix socket)", socketPath)
	return srv.Serve(listener)
}

// Shutdown gracefully shuts down the server (no-op in logical time mode).
func (s *Server) Shutdown() {
	// No background goroutines to stop in logical time mode
}
