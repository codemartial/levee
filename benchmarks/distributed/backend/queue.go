// Package backend implements the backend simulator for the distributed benchmark.
package backend

import (
	"sync"
	"time"
)

// QueueStatus represents the outcome of a queue operation.
type QueueStatus int

const (
	QueueStatusProcessed QueueStatus = iota // Request was queued and would complete in time
	QueueStatusTimeout                      // Request would timeout while queued
	QueueStatusShed                         // Queue is full, request rejected (503)
)

// QueueResult is the outcome of attempting to queue a request.
type QueueResult struct {
	Status    QueueStatus
	QueueWait time.Duration // How long the request waited in queue (0 if shed)
}

// RequestQueue implements a logical-time FIFO queue.
// Instead of actually holding requests, it tracks when capacity becomes available.
type RequestQueue struct {
	mu sync.Mutex

	// Logical time when the queue will be drained (last request completes)
	drainTimeNS int64

	// Current queue depth (number of conceptual items waiting)
	depth int

	// Max queue size (10% of capacity)
	maxDepth int

	// Average processing time per request (for queue wait estimation)
	// This is updated based on capacity
	processingTimePerRequestNS int64

	// Metrics
	DroppedExpired  int64
	DroppedOverflow int64
}

// NewRequestQueue creates a new logical-time request queue.
func NewRequestQueue(maxDepth int) *RequestQueue {
	if maxDepth < 1 {
		maxDepth = 1
	}
	return &RequestQueue{
		maxDepth: maxDepth,
		// Default processing time: 1s / 150 RPS = ~6.67ms per request
		processingTimePerRequestNS: int64(6666666),
	}
}

// TryQueue attempts to queue a request at the given logical time.
// Returns immediately with the result (no waiting).
func (q *RequestQueue) TryQueue(now time.Time, timeout, processingTime time.Duration) QueueResult {
	q.mu.Lock()
	defer q.mu.Unlock()

	nowNS := now.UnixNano()

	// If drain time is in the past, reset it to now
	if q.drainTimeNS < nowNS {
		q.drainTimeNS = nowNS
		q.depth = 0
	}

	// Check if queue is full
	if q.depth >= q.maxDepth {
		q.DroppedOverflow++
		return QueueResult{Status: QueueStatusShed}
	}

	// Calculate when this request would start processing
	queueWaitNS := q.drainTimeNS - nowNS
	if queueWaitNS < 0 {
		queueWaitNS = 0
	}

	// Calculate total time (queue wait + processing)
	totalTimeNS := queueWaitNS + processingTime.Nanoseconds()

	// Check if request would timeout
	if totalTimeNS >= timeout.Nanoseconds() {
		q.DroppedExpired++
		return QueueResult{
			Status:    QueueStatusTimeout,
			QueueWait: time.Duration(queueWaitNS),
		}
	}

	// Request can be processed - update queue state
	q.depth++
	q.drainTimeNS = nowNS + totalTimeNS

	return QueueResult{
		Status:    QueueStatusProcessed,
		QueueWait: time.Duration(queueWaitNS),
	}
}

// UpdateMaxDepth dynamically adjusts queue capacity (called when replicas change).
func (q *RequestQueue) UpdateMaxDepth(newMax int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if newMax < 1 {
		newMax = 1
	}
	q.maxDepth = newMax
}

// Len returns the current queue depth.
func (q *RequestQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// MaxDepth returns the current max queue depth.
func (q *RequestQueue) MaxDepth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.maxDepth
}

// DrainToTime advances the queue to the given logical time, clearing completed items.
func (q *RequestQueue) DrainToTime(now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()

	nowNS := now.UnixNano()
	if q.drainTimeNS <= nowNS {
		q.depth = 0
		q.drainTimeNS = nowNS
	}
}
