package backend

import (
	"container/heap"
	"math"
	"sync"
	"time"
)

// ProcessStatus represents the outcome of a worker pool processing attempt.
type ProcessStatus int

const (
	ProcessOK      ProcessStatus = iota // Processed (possibly with degradation)
	ProcessTimeout                      // Would timeout (queue wait + processing > timeout)
	ProcessShed                         // Queue full, rejected
	ProcessCrashed                      // Node is crashed, all requests fail
)

// ProcessResult is the outcome of attempting to process a request through the worker pool.
type ProcessResult struct {
	Status        ProcessStatus
	QueueWait     int64   // nanoseconds waited in queue (0 if immediate or shed)
	LatencyScale  float64 // multiplier applied to nominal latency (>= 1.0)
	LoadErrorRate float64 // additional error probability [0, 1) from load-driven degradation
}

// pendingItem represents a queued request waiting for a worker.
type pendingItem struct {
	durationNS int64 // scaled processing duration
}

// WorkerPool models a concurrency-limited backend with load-dependent degradation.
//
// Workers are tracked via a min-heap of busyUntilNS timestamps — one per
// concurrent processing slot. When a request arrives, the earliest-free worker
// is found (heap root). If free, the request processes immediately. If all
// workers are busy, the request queues behind the earliest-completing worker.
//
// The ratio of active work (busy workers + queued items) to total slots defines
// the instantaneous "load". Load drives two degradation curves:
//   - Latency multiplier: inflates nominal processing time as load increases.
//   - Error rate: adds failures on top of baseline as resources exhaust.
//
// Sustained extreme load triggers crash simulation — all requests fail for a
// restart period, then the pool recovers.
type WorkerPool struct {
	mu sync.Mutex

	// Worker slots: min-heap of busyUntilNS values.
	// A slot with busyUntilNS <= nowNS is "free".
	workers workerHeap

	// Pending queue: FIFO of items waiting for a worker to become free.
	pending []pendingItem

	// Configuration
	slotsPerReplica int
	maxQueueDepth   int

	// EWMA load tracking (for error escalation and crash decisions).
	// Uses time-weighted decay with ~2s half-life.
	ewmaLoad   float64
	ewmaLastNS int64

	// Crash simulation state
	crashed        bool
	crashRecoverNS int64 // logical time when crash recovery completes

	// Sustained overload tracking for crash trigger
	overloadStartNS int64 // when EWMA first exceeded crash threshold (0 = not overloading)

	// Metrics
	DroppedOverflow int64
	DroppedTimeout  int64
	totalCrashes    int64
}

const (
	// EWMA half-life for load smoothing (5 seconds).
	ewmaHalfLifeNS = int64(5 * 1e9)

	// Crash triggers when EWMA load exceeds this for sustainedOverloadNS.
	crashLoadThreshold = 3.0

	// Duration of sustained extreme load before crash triggers (30 seconds).
	crashSustainedNS = int64(30 * 1e9)

	// How long a crashed node stays down (5 minutes).
	crashRestartNS = int64(300 * 1e9)

	// Degradation thresholds
	latencyDegradationStart = 0.8 // load above which latency inflation begins
	errorDegradationStart   = 1.0 // load above which extra errors begin
)

// NewWorkerPool creates a worker pool sized by Little's Law.
//
//	slotsPerReplica = ceil(throughputRPS × meanLatencySeconds)
//	totalSlots = slotsPerReplica × replicas
func NewWorkerPool(slotsPerReplica, replicas, maxQueueDepth int) *WorkerPool {
	totalSlots := slotsPerReplica * replicas
	if totalSlots < 1 {
		totalSlots = 1
	}
	if maxQueueDepth < 1 {
		maxQueueDepth = 1
	}

	wp := &WorkerPool{
		slotsPerReplica: slotsPerReplica,
		maxQueueDepth:   maxQueueDepth,
		workers:         make(workerHeap, totalSlots),
	}
	heap.Init(&wp.workers)
	return wp
}

// TryProcess attempts to process a request at the given logical time.
//
// Worker occupancy uses the nominal processing time (constant throughput).
// The degradation multiplier affects only reported latency and error rates,
// not the rate at which workers drain. This models real servers where CPU
// processing is relatively constant but the client-perceived latency
// increases due to contention overhead, GC pauses, and queuing.
//
// Returns a ProcessResult describing the outcome. For ProcessOK, the caller
// should compute total latency as QueueWait + (nominalProcessingNS × LatencyScale)
// and roll against LoadErrorRate for additional failures.
func (wp *WorkerPool) TryProcess(nowNS int64, nominalProcessingNS int64, timeoutNS int64) ProcessResult {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// Check crash state — no auto-recovery; caller must use TryRecover
	if wp.crashed {
		return ProcessResult{Status: ProcessCrashed}
	}

	numSlots := len(wp.workers)
	if numSlots == 0 {
		return ProcessResult{Status: ProcessCrashed}
	}

	// Drain pending items into workers that have become free
	wp.drainPending(nowNS)

	// Compute instantaneous load and update EWMA
	busyWorkers := wp.countBusy(nowNS)
	queueDepth := len(wp.pending)
	load := float64(busyWorkers+queueDepth) / float64(numSlots)

	wp.updateEWMA(nowNS, load)
	if wp.checkCrash(nowNS) {
		return ProcessResult{Status: ProcessCrashed}
	}

	// Degradation uses EWMA-smoothed load, not instantaneous.
	// Instantaneous load spikes during normal burst arrivals (exponential
	// inter-arrival creates micro-bursts); EWMA filters these out while
	// still responding to sustained overload.
	latencyScale := latencyDegradationMultiplier(wp.ewmaLoad)
	scaledProcessingNS := int64(float64(nominalProcessingNS) * latencyScale)
	loadErrorRate := loadDegradationErrorRate(wp.ewmaLoad)

	// Dispatch: find earliest-free worker
	earliestFreeNS := wp.workers[0] // heap root = min busyUntilNS

	if earliestFreeNS <= nowNS {
		// Worker is free — process immediately.
		// Worker occupancy uses NOMINAL processing time (constant throughput).
		wp.workers[0] = nowNS + nominalProcessingNS
		heap.Fix(&wp.workers, 0)

		// Timeout check uses SCALED processing time (client-perceived)
		if scaledProcessingNS >= timeoutNS {
			wp.DroppedTimeout++
			return ProcessResult{
				Status:       ProcessTimeout,
				QueueWait:    0,
				LatencyScale: latencyScale,
			}
		}

		return ProcessResult{
			Status:        ProcessOK,
			QueueWait:     0,
			LatencyScale:  latencyScale,
			LoadErrorRate: loadErrorRate,
		}
	}

	// All workers busy — check queue capacity
	if queueDepth >= wp.maxQueueDepth {
		wp.DroppedOverflow++
		return ProcessResult{Status: ProcessShed}
	}

	// Compute queue wait by simulating forward through pending items
	startNS := wp.simulateStartTime()
	queueWaitNS := startNS - nowNS
	if queueWaitNS < 0 {
		queueWaitNS = 0
	}

	// Enqueue with NOMINAL processing time (constant throughput)
	wp.pending = append(wp.pending, pendingItem{durationNS: nominalProcessingNS})

	// Timeout check uses queue wait + SCALED processing time
	totalNS := queueWaitNS + scaledProcessingNS
	if totalNS >= timeoutNS {
		wp.DroppedTimeout++
		return ProcessResult{
			Status:       ProcessTimeout,
			QueueWait:    queueWaitNS,
			LatencyScale: latencyScale,
		}
	}

	return ProcessResult{
		Status:        ProcessOK,
		QueueWait:     queueWaitNS,
		LatencyScale:  latencyScale,
		LoadErrorRate: loadErrorRate,
	}
}

// Resize adjusts the worker pool to match the current replica count.
// Called after each HPA tick.
func (wp *WorkerPool) Resize(replicas int, maxQueueDepth int) {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	targetSlots := wp.slotsPerReplica * replicas
	if targetSlots < 1 {
		targetSlots = 1
	}

	currentSlots := len(wp.workers)

	if targetSlots > currentSlots {
		// Scale up: add free workers
		for i := currentSlots; i < targetSlots; i++ {
			heap.Push(&wp.workers, int64(0))
		}
	} else if targetSlots < currentSlots {
		// Scale down: remove earliest-free workers (heap.Pop removes min)
		for len(wp.workers) > targetSlots {
			heap.Pop(&wp.workers)
		}
	}

	if maxQueueDepth < 1 {
		maxQueueDepth = 1
	}
	wp.maxQueueDepth = maxQueueDepth
}

// --- Degradation curves ---

// latencyDegradationMultiplier returns the latency inflation factor for a given
// instantaneous load. Piecewise linear:
//
//	≤0.8:    1.0×  (healthy)
//	0.8→1.0: 1.0×→1.5×  (contention begins)
//	1.0→1.5: 1.5×→3.0×  (GC pressure, connection pool exhaustion)
//	1.5→2.5: 3.0×→10.0× (thrashing)
//	>2.5:    10.0× (capped; crash sim handles the rest)
func latencyDegradationMultiplier(load float64) float64 {
	switch {
	case load <= latencyDegradationStart:
		return 1.0
	case load <= 1.0:
		t := (load - 0.8) / 0.2
		return 1.0 + t*0.5
	case load <= 1.5:
		t := (load - 1.0) / 0.5
		return 1.5 + t*1.5
	case load <= 2.5:
		t := (load - 1.5) / 1.0
		return 3.0 + t*7.0
	default:
		return 10.0
	}
}

// loadDegradationErrorRate returns the additional error probability for a given
// EWMA-smoothed load. Additive on top of baseline error rate.
//
//	≤1.0:    0%     (within capacity)
//	1.0→1.5: 0%→10% (connection pool pressure, thread starvation)
//	1.5→2.5: 10%→40% (OOM risk, cascading failures)
//	>2.5:    40%    (pre-crash)
func loadDegradationErrorRate(ewmaLoad float64) float64 {
	switch {
	case ewmaLoad <= errorDegradationStart:
		return 0.0
	case ewmaLoad <= 1.5:
		t := (ewmaLoad - 1.0) / 0.5
		return t * 0.10
	case ewmaLoad <= 2.5:
		t := (ewmaLoad - 1.5) / 1.0
		return 0.10 + t*0.30
	default:
		return 0.40
	}
}

// --- Internal helpers ---

// drainPending assigns queued items to workers that have become free.
func (wp *WorkerPool) drainPending(nowNS int64) {
	drained := 0
	for drained < len(wp.pending) {
		if wp.workers[0] > nowNS {
			break // no worker free yet
		}
		// Earliest worker is free; assign next pending item
		item := wp.pending[drained]
		startNS := wp.workers[0]
		if startNS < nowNS {
			startNS = nowNS
		}
		wp.workers[0] = startNS + item.durationNS
		heap.Fix(&wp.workers, 0)
		drained++
	}
	if drained > 0 {
		wp.pending = wp.pending[drained:]
	}
}

// countBusy returns the number of workers with busyUntilNS > nowNS.
func (wp *WorkerPool) countBusy(nowNS int64) int {
	count := 0
	for _, busyUntil := range wp.workers {
		if busyUntil > nowNS {
			count++
		}
	}
	return count
}

// simulateStartTime computes when the next queued request would start
// processing by replaying all pending items into a temporary heap copy.
func (wp *WorkerPool) simulateStartTime() int64 {
	if len(wp.pending) == 0 {
		return wp.workers[0]
	}

	// Copy the worker heap
	tmp := make(workerHeap, len(wp.workers))
	copy(tmp, wp.workers)

	// Replay all pending items
	for _, item := range wp.pending {
		startNS := tmp[0]
		tmp[0] = startNS + item.durationNS
		heapFixRoot(tmp)
	}

	return tmp[0]
}

// heapFixRoot restores heap invariant after modifying index 0 (sift-down).
func heapFixRoot(h workerHeap) {
	n := len(h)
	i := 0
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		j := left
		if right := left + 1; right < n && h[right] < h[left] {
			j = right
		}
		if h[i] <= h[j] {
			break
		}
		h[i], h[j] = h[j], h[i]
		i = j
	}
}

// updateEWMA updates the exponentially weighted moving average of load.
func (wp *WorkerPool) updateEWMA(nowNS int64, instantLoad float64) {
	if wp.ewmaLastNS == 0 {
		wp.ewmaLoad = instantLoad
		wp.ewmaLastNS = nowNS
		return
	}

	dtNS := nowNS - wp.ewmaLastNS
	if dtNS <= 0 {
		return
	}

	// alpha = 1 - exp(-dt * ln2 / halfLife)
	alpha := 1.0 - math.Exp(-float64(dtNS)*0.693147/float64(ewmaHalfLifeNS))
	wp.ewmaLoad = alpha*instantLoad + (1-alpha)*wp.ewmaLoad
	wp.ewmaLastNS = nowNS
}

// checkCrash checks if sustained extreme load should trigger a crash.
// Returns true if a crash was just triggered.
func (wp *WorkerPool) checkCrash(nowNS int64) bool {
	if wp.ewmaLoad >= crashLoadThreshold {
		if wp.overloadStartNS == 0 {
			wp.overloadStartNS = nowNS
		} else if nowNS-wp.overloadStartNS >= crashSustainedNS {
			// Trigger crash
			wp.crashed = true
			wp.crashRecoverNS = nowNS + crashRestartNS
			wp.overloadStartNS = 0
			wp.totalCrashes++
			// All workers become busy until recovery
			for i := range wp.workers {
				wp.workers[i] = wp.crashRecoverNS
			}
			heap.Init(&wp.workers)
			wp.pending = wp.pending[:0]
			return true
		}
	} else {
		wp.overloadStartNS = 0
	}
	return false
}

// recoverFromCrash resets the pool after a crash.
func (wp *WorkerPool) recoverFromCrash() {
	wp.crashed = false
	for i := range wp.workers {
		wp.workers[i] = 0
	}
	heap.Init(&wp.workers)
	wp.pending = wp.pending[:0]
	wp.ewmaLoad = 0
	wp.overloadStartNS = 0
}

// --- Exported query methods ---

// NumSlots returns the current number of worker slots.
func (wp *WorkerPool) NumSlots() int {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return len(wp.workers)
}

// QueueDepth returns the current number of pending items.
func (wp *WorkerPool) QueueDepth() int {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return len(wp.pending)
}

// IsCrashed returns true if the pool is in a crashed state.
func (wp *WorkerPool) IsCrashed() bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.crashed
}

// TryRecover checks if the crash recovery period has elapsed.
// Returns true if the pool just recovered, false if still crashed.
func (wp *WorkerPool) TryRecover(nowNS int64) bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if !wp.crashed {
		return false
	}
	if nowNS < wp.crashRecoverNS {
		return false
	}
	wp.recoverFromCrash()
	return true
}

// TotalCrashes returns the cumulative number of crashes.
func (wp *WorkerPool) TotalCrashes() int64 {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.totalCrashes
}

// EWMALoad returns the current EWMA load estimate.
func (wp *WorkerPool) EWMALoad() float64 {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.ewmaLoad
}

// --- workerHeap implements container/heap.Interface ---

type workerHeap []int64

func (h workerHeap) Len() int            { return len(h) }
func (h workerHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h workerHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *workerHeap) Push(x any)         { *h = append(*h, x.(int64)) }
func (h *workerHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// --- Time helper ---

// DurationNS converts a time.Duration to nanoseconds as int64.
func DurationNS(d time.Duration) int64 {
	return d.Nanoseconds()
}
