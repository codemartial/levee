package mesh

import (
	"container/heap"
	"math"
	"time"

	"github.com/codemartial/levee/benchmarks/distributed/backend"
)

// workerHeap is a min-heap of per-slot busyUntilNS times.
type workerHeap []int64

func (h workerHeap) Len() int            { return len(h) }
func (h workerHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h workerHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *workerHeap) Push(x any) { *h = append(*h, x.(int64)) }
func (h *workerHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// nodeRuntime is the live state of one mesh node: elastic capacity with a
// crash-on-queue-saturation failure mode. Physical occupancy (backlog) is
// decoupled from logical request outcomes.
type nodeRuntime struct {
	idx             int
	spec            NodeSpec
	cc              *backend.CapacityController
	workers         workerHeap
	slotsPerReplica int
	backlog         int // executing + queued; the concurrency metric
	profile         latencyProfile
	gov             Governor

	crashed        bool
	crashedSinceNS int64
	gen            uint64 // bumped on crash; stale completions carry old gen
	govReplicas    int    // governor instance count, synced to replicas

	// Metrics accumulators; downtime is tracked engine-side with end-clamping.
	crashCount int
	peakConc   int
	concSumNS  float64 // integral of backlog over time
	lastConcNS int64
}

func newNodeRuntime(idx int, spec NodeSpec, gov Governor) *nodeRuntime {
	profile := newLatencyProfile(spec.P50MS, spec.P99MS, defaultTimeoutMS)
	cc := backend.NewCapacityController(backend.CapacityControllerConfig{
		BaseShape: backend.BaseShape{
			ThroughputRPS: spec.PerReplicaRPS,
			QueueDepth:    bufferSeconds * spec.PerReplicaRPS,
		},
		MinReplicas:            spec.MinReplicas,
		MaxReplicas:            spec.MaxReplicas,
		TargetUtilization:      0.70,
		EvaluationInterval:     15 * time.Second,
		ScaleDownStabilization: 300 * time.Second,
		ScaleDownDelay:         600 * time.Second,
		ProvisioningLag:        30 * time.Second,
	})
	n := &nodeRuntime{
		idx:             idx,
		spec:            spec,
		cc:              cc,
		slotsPerReplica: slotsPerReplica(spec.PerReplicaRPS, profile.meanMS()),
		profile:         profile,
		gov:             gov,
		govReplicas:     spec.MinReplicas, // candidates construct at MinReplicas
	}
	n.resize(0)
	return n
}

// syncGov scales governor instances with active replicas; a crashed node
// has zero live instances.
func (n *nodeRuntime) syncGov(tsNS int64) {
	r := 0
	if !n.crashed {
		r = n.cc.CurrentReplicas()
	}
	if r != n.govReplicas {
		n.gov.Resize(r, tsNS)
		n.govReplicas = r
	}
}

// slotsPerReplica sizes worker concurrency by Little's Law.
func slotsPerReplica(rps int, meanMS float64) int {
	s := int(math.Ceil(float64(rps) * meanMS / 1000.0))
	if s < 1 {
		s = 1
	}
	return s
}

// resize grows or shrinks the worker heap to match current replicas.
// Shrinking drops the most-idle slots first; near-unused since scale-down
// is effectively disabled by ScaleDownDelay over the sim horizon.
func (n *nodeRuntime) resize(nowNS int64) {
	target := n.slotsPerReplica * n.cc.CurrentReplicas()
	for len(n.workers) < target {
		heap.Push(&n.workers, nowNS)
	}
	for len(n.workers) > target && len(n.workers) > 0 {
		heap.Pop(&n.workers)
	}
}

// bufferLimit is 5 seconds' worth of demand at instantaneous capacity.
func (n *nodeRuntime) bufferLimit() int {
	return bufferSeconds * n.spec.PerReplicaRPS * n.cc.CurrentReplicas()
}

// admit schedules serviceNS of work on the least-busy slot, FIFO through the
// slot heap; returns the completion time including queue wait.
func (n *nodeRuntime) admit(nowNS, serviceNS int64) int64 {
	start := n.workers[0]
	if start < nowNS {
		start = nowNS
	}
	done := start + serviceNS
	n.workers[0] = done
	heap.Fix(&n.workers, 0)
	return done
}

// updateConc integrates backlog over time; call BEFORE changing backlog.
func (n *nodeRuntime) updateConc(nowNS int64) {
	if nowNS > n.lastConcNS {
		n.concSumNS += float64(n.backlog) * float64(nowNS-n.lastConcNS)
		n.lastConcNS = nowNS
	}
}

// crash drops all in-flight and queued work and kills all governor
// instances; upstreams learn only via timeouts.
func (n *nodeRuntime) crash(nowNS int64) {
	n.updateConc(nowNS)
	n.crashed = true
	n.crashedSinceNS = nowNS
	n.gen++
	n.crashCount++
	n.backlog = 0
	n.workers = n.workers[:0]
	n.syncGov(nowNS)
}

// recover restarts the node at MinReplicas with fresh governor instances;
// HPA retains its demand estimate.
func (n *nodeRuntime) recover(nowNS int64) {
	n.crashed = false
	n.cc.ResetAfterCrash()
	n.resize(nowNS)
	n.syncGov(nowNS)
}
