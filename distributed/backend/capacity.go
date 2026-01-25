package backend

import (
	"math"
	"sync"
	"time"
)

// BaseShape defines the capacity of a single replica.
type BaseShape struct {
	ThroughputRPS int // e.g., 150 RPS per replica
	QueueDepth    int // e.g., 15 (10% of throughput)
}

// DefaultBaseShape returns the default base shape (150 RPS + 15 queue).
func DefaultBaseShape() BaseShape {
	return BaseShape{
		ThroughputRPS: 150,
		QueueDepth:    15,
	}
}

// CapacityControllerConfig holds configuration for the autoscaler.
type CapacityControllerConfig struct {
	BaseShape              BaseShape
	MinReplicas            int
	MaxReplicas            int
	TargetUtilization      float64       // e.g., 0.70 for 70%
	EvaluationInterval     time.Duration // e.g., 15s
	ScaleDownStabilization time.Duration // e.g., 300s (5 minutes)
	ProvisioningLag        time.Duration // e.g., 30s
}

// DefaultCapacityControllerConfig returns sensible defaults.
func DefaultCapacityControllerConfig() CapacityControllerConfig {
	return CapacityControllerConfig{
		BaseShape:              DefaultBaseShape(),
		MinReplicas:            1,
		MaxReplicas:            8,
		TargetUtilization:      0.70,
		EvaluationInterval:     15 * time.Second,
		ScaleDownStabilization: 300 * time.Second,
		ProvisioningLag:        30 * time.Second,
	}
}

// utilizationSample records utilization at a point in time.
type utilizationSample struct {
	timestamp   time.Time
	utilization float64
}

// CapacityController implements HPA-style autoscaling.
type CapacityController struct {
	mu     sync.RWMutex
	config CapacityControllerConfig

	currentReplicas int // Active replicas
	pendingReplicas int // Replicas being provisioned

	// State tracking
	lastEvaluation     time.Time
	utilizationHistory []utilizationSample
	pendingReadyAt     time.Time

	// RPS tracking (EWMA with 5s half-life)
	// Uses min/max timestamps to handle out-of-order concurrent requests
	currentRPS     float64
	minTimestamp   time.Time // Earliest timestamp in current window
	maxTimestamp   time.Time // Latest timestamp in current window
	requestCounter int64
}

// NewCapacityController creates a new capacity controller.
func NewCapacityController(config CapacityControllerConfig) *CapacityController {
	return &CapacityController{
		config:          config,
		currentReplicas: config.MinReplicas,
	}
}

// CurrentCapacity returns the current throughput capacity in RPS.
func (c *CapacityController) CurrentCapacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config.BaseShape.ThroughputRPS * c.currentReplicas
}

// CurrentQueueDepth returns the current max queue depth.
func (c *CapacityController) CurrentQueueDepth() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config.BaseShape.QueueDepth * c.currentReplicas
}

// CurrentReplicas returns the number of active replicas.
func (c *CapacityController) CurrentReplicas() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentReplicas
}

// PendingReplicas returns the number of replicas being provisioned.
func (c *CapacityController) PendingReplicas() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pendingReplicas
}

// GetCurrentRPS returns the current RPS estimate.
func (c *CapacityController) GetCurrentRPS() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentRPS
}

// RecordRequest records an incoming request for RPS tracking.
// Uses min/max timestamps to correctly handle out-of-order concurrent requests.
func (c *CapacityController) RecordRequest(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.requestCounter++

	// Track min/max timestamps to handle out-of-order arrivals
	if c.minTimestamp.IsZero() || now.Before(c.minTimestamp) {
		c.minTimestamp = now
	}
	if c.maxTimestamp.IsZero() || now.After(c.maxTimestamp) {
		c.maxTimestamp = now
	}

	// Update EWMA when we have at least 1 second of logical time span
	elapsed := c.maxTimestamp.Sub(c.minTimestamp).Seconds()
	if elapsed >= 1.0 {
		// Calculate RPS for this period
		instantRPS := float64(c.requestCounter) / elapsed

		// EWMA with alpha = 0.3 (approximately 5s half-life)
		alpha := 0.3
		c.currentRPS = alpha*instantRPS + (1-alpha)*c.currentRPS

		// Reset window starting from current timestamp
		c.requestCounter = 0
		c.minTimestamp = now
		c.maxTimestamp = now
	}
}

// Tick evaluates autoscaling decisions. Should be called periodically.
func (c *CapacityController) Tick(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Promote pending replicas if provisioning is complete
	if c.pendingReplicas > 0 && now.After(c.pendingReadyAt) {
		c.currentReplicas += c.pendingReplicas
		c.pendingReplicas = 0
	}

	// Only evaluate at the configured interval
	if !c.lastEvaluation.IsZero() && now.Sub(c.lastEvaluation) < c.config.EvaluationInterval {
		return
	}
	c.lastEvaluation = now

	// Calculate current utilization
	capacity := float64(c.config.BaseShape.ThroughputRPS * c.currentReplicas)
	if capacity == 0 {
		return
	}
	utilization := c.currentRPS / capacity

	// Record utilization sample
	c.utilizationHistory = append(c.utilizationHistory, utilizationSample{now, utilization})

	// Prune old samples outside stabilization window
	cutoff := now.Add(-c.config.ScaleDownStabilization)
	for len(c.utilizationHistory) > 0 && c.utilizationHistory[0].timestamp.Before(cutoff) {
		c.utilizationHistory = c.utilizationHistory[1:]
	}

	// Calculate target replicas
	targetReplicas := int(math.Ceil(c.currentRPS / (c.config.TargetUtilization * float64(c.config.BaseShape.ThroughputRPS))))
	targetReplicas = max(c.config.MinReplicas, min(c.config.MaxReplicas, targetReplicas))

	totalReplicas := c.currentReplicas + c.pendingReplicas

	if targetReplicas > totalReplicas {
		// Scale up: immediate decision, but provisioning takes time
		toAdd := targetReplicas - totalReplicas
		c.pendingReplicas += toAdd
		c.pendingReadyAt = now.Add(c.config.ProvisioningLag)

	} else if targetReplicas < c.currentReplicas {
		// Scale down: only if ALL samples in window are below threshold
		maxUtil := 0.0
		for _, s := range c.utilizationHistory {
			maxUtil = max(maxUtil, s.utilization)
		}
		// Require 10% buffer below target to scale down
		if maxUtil < c.config.TargetUtilization*0.9 {
			toRemove := min(c.currentReplicas-targetReplicas, c.currentReplicas-c.config.MinReplicas)
			if toRemove > 0 {
				c.currentReplicas -= toRemove
			}
		}
	}
}

// Status returns the current autoscaler status for monitoring.
type CapacityStatus struct {
	CurrentReplicas int
	PendingReplicas int
	CapacityRPS     int
	QueueDepth      int
	CurrentRPS      float64
	Utilization     float64
}

// Status returns the current capacity status.
func (c *CapacityController) Status() CapacityStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	capacity := c.config.BaseShape.ThroughputRPS * c.currentReplicas
	var util float64
	if capacity > 0 {
		util = c.currentRPS / float64(capacity)
	}

	return CapacityStatus{
		CurrentReplicas: c.currentReplicas,
		PendingReplicas: c.pendingReplicas,
		CapacityRPS:     capacity,
		QueueDepth:      c.config.BaseShape.QueueDepth * c.currentReplicas,
		CurrentRPS:      c.currentRPS,
		Utilization:     util,
	}
}
