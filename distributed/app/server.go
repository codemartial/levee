package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/distributed/api"
	"github.com/codemartial/loadgen"
)

// Server is the application HTTP server that orchestrates circuit breakers.
type Server struct {
	orchestrator *Orchestrator
	specs        []loadgen.LoadSpec
}

// ServerConfig holds configuration for the app server.
type ServerConfig struct {
	BackendHost string
	SLO         levee.SLO
	Specs       []loadgen.LoadSpec
	CBName      string // If set, only run this CB (for isolated testing)
}

// NewServer creates a new application server.
func NewServer(config ServerConfig) *Server {
	return &Server{
		orchestrator: NewOrchestrator(OrchestratorConfig{
			BackendHost: config.BackendHost,
			SLO:         config.SLO,
			Specs:       config.Specs,
			StartTime:   time.Unix(0, 0), // Logical time starts at epoch
			CBName:      config.CBName,
		}),
		specs: config.Specs,
	}
}

// HandleRequest handles POST /request from the load generator.
func (s *Server) HandleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req api.AppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutMS*2)*time.Millisecond)
	defer cancel()

	resp := s.orchestrator.HandleRequest(ctx, req)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleMetrics handles GET /metrics.
func (s *Server) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metrics := s.orchestrator.GetMetrics()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

// HandleHealth handles GET /health.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// Handler returns an http.Handler for the app server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/request", s.HandleRequest)
	mux.HandleFunc("/metrics", s.HandleMetrics)
	mux.HandleFunc("/health", s.HandleHealth)
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
	log.Printf("App server starting on %s (TCP)", addr)
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

	log.Printf("App server starting on %s (Unix socket)", socketPath)
	return srv.Serve(listener)
}
