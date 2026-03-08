// Package api defines shared types for the distributed benchmark system.
package api

import "time"

// AppRequest is sent from Load Generator to Application container.
type AppRequest struct {
	RequestID   string `json:"request_id"`
	TimestampNS int64  `json:"timestamp_ns"`
	SpecIndex   int    `json:"spec_index"`
	TimeoutMS   int    `json:"timeout_ms"`
}

// AppResponse is returned from Application to Load Generator.
type AppResponse struct {
	RequestID string              `json:"request_id"`
	Results   map[string]CBResult `json:"results"` // keyed by CB name
}

// CBResult contains the result of a single circuit breaker's handling of a request.
type CBResult struct {
	Allowed bool   `json:"allowed"`
	State   string `json:"state"`
}

// BackendRequest is sent from Application to Backend container.
type BackendRequest struct {
	RequestID   string `json:"request_id"`
	CBName      string `json:"cb_name"`
	TimestampNS int64  `json:"timestamp_ns"`
	TimeoutMS   int    `json:"timeout_ms"`
	SpecIndex   int    `json:"spec_index"`
}

// Completion represents the outcome of a backend request, delivered when
// the request's logical completion time has elapsed. The backend is
// authoritative about when and how requests complete.
type Completion struct {
	CompletionNS int64
	Latency      time.Duration
	Success      bool
}

// AppMetrics is returned by GET /metrics on the application.
type AppMetrics struct {
	CircuitBreakers map[string]CBMetrics `json:"circuit_breakers"`
	TotalRequests   int64                `json:"total_requests"`
	ElapsedSeconds  float64              `json:"elapsed_seconds"`
}

// CBMetrics contains per-CB aggregate metrics.
type CBMetrics struct {
	State            string  `json:"state"`
	TotalAllowed     int64   `json:"total_allowed"`
	TotalBlocked     int64   `json:"total_blocked"`
	TotalSuccesses   int64   `json:"total_successes"`
	TotalFailures    int64   `json:"total_failures"`
	StateTransitions int     `json:"state_transitions"`
	SuccessScore     float64 `json:"success_score"`     // sqrt(sum of epoch scores)
	FailureScore     float64 `json:"failure_score"`     // sqrt(sum of epoch scores)
	MaxConcurrency   int64   `json:"max_concurrency"`   // max in-flight while NOT OPEN
}

// LoadGenStatus is returned by GET /status on the load generator.
type LoadGenStatus struct {
	CurrentSpecIndex  int     `json:"current_spec_index"`
	ElapsedSeconds    float64 `json:"elapsed_seconds"`       // Wall clock elapsed
	LogicalTimeHours  float64 `json:"logical_time_hours"`    // Simulated time elapsed
	TotalRequests     int64   `json:"total_requests"`
	CurrentRPS        float64 `json:"current_rps"`           // Wall clock RPS
	TotalLogicalHours float64 `json:"total_logical_hours"`   // Total simulation duration
}
