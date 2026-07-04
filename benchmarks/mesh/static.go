package mesh

import (
	"math"

	"github.com/codemartial/levee/benchmarks"
)

// Static sizing derivations (no scenario foreknowledge, design numbers only):
//
// Inbound token bucket at node N (one per node -- distributed rate limiting):
//   rate = headroom * steadyInboundRPS(N)
// A common rule of thumb provisions admission limits ~50% above design load,
// so headroom = 1.5. burst = 1 second of tokens, absorbing Poisson
// micro-bursts without admitting sustained overload.
//
// Per-replica outbound concurrency limiter on edge N->M, via Little's Law:
//   perInstanceMaxInflight =
//     ceil(headroom * steadyEdgeRPS * subtreeMeanLatencyS(M) / designReplicas(N))
// where steadyEdgeRPS = p_edge * steadyInboundRPS(N), designReplicas is the
// replica count the topology needs at steady state, and the subtree mean is
// the expected end-to-end latency of a call into M (local mean plus deepest
// probability-weighted sync branch, see Topology.SubtreeMeanLatencyMS).
// Floor of 2 so probing is never single-file.
//
// Per-replica outbound breaker config reuses the existing static configs by
// the per-instance traffic volume each breaker actually sees, matching the
// documented derivation ranges in static_cb.go: BAU for <= 800 RPS,
// Peak above 800 RPS.
const (
	staticHeadroom    = 1.5
	peakConfigMinRPS  = 800.0
)

// TokenBucket is a fixed-rate inbound limiter on synthetic time.
type TokenBucket struct {
	ratePerSec float64
	burst      float64
	tokens     float64
	lastNS     int64
}

// NewTokenBucket starts full so warmup traffic is not spuriously rejected.
func NewTokenBucket(ratePerSec, burst float64) *TokenBucket {
	return &TokenBucket{ratePerSec: ratePerSec, burst: burst, tokens: burst}
}

func (tb *TokenBucket) refill(tsNS int64) {
	if tsNS > tb.lastNS {
		tb.tokens = math.Min(tb.burst, tb.tokens+tb.ratePerSec*float64(tsNS-tb.lastNS)/1e9)
		tb.lastNS = tsNS
	}
}

// Allow takes one token if available.
func (tb *TokenBucket) Allow(tsNS int64) bool {
	tb.refill(tsNS)
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

// Saturated reports whether the bucket cannot admit a request right now.
func (tb *TokenBucket) Saturated(tsNS int64) bool {
	tb.refill(tsNS)
	return tb.tokens < 1
}

// ConcurrencyLimiter is a fixed max-inflight outbound limiter.
type ConcurrencyLimiter struct {
	max      int
	inflight int
}

func NewConcurrencyLimiter(max int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{max: max}
}

func (cl *ConcurrencyLimiter) TryAcquire() bool {
	if cl.inflight >= cl.max {
		return false
	}
	cl.inflight++
	return true
}

func (cl *ConcurrencyLimiter) Release() {
	if cl.inflight > 0 {
		cl.inflight--
	}
}

func (cl *ConcurrencyLimiter) Saturated() bool { return cl.inflight >= cl.max }

// staticSizing holds the derived per-node limits, computed once so every
// replica instance is configured identically.
type staticSizing struct {
	inboundRate     float64
	edgeMaxInflight []int
	edgeCBConfig    []benchmarks.StaticCBConfig
}

// designReplicas is the replica count the node needs at the given load.
func designReplicas(spec NodeSpec, rps float64) int {
	r := int(math.Ceil(rps / (0.70 * float64(spec.PerReplicaRPS))))
	if r < spec.MinReplicas {
		r = spec.MinReplicas
	}
	if r > spec.MaxReplicas {
		r = spec.MaxReplicas
	}
	return r
}

// staticNodeSizing derives all static limits for one node from the topology
// design numbers in steady.
func staticNodeSizing(topo *Topology, steady map[string]float64, nodeIdx int) staticSizing {
	n := topo.Nodes[nodeIdx]
	replicas := designReplicas(n, steady[n.Name])
	s := staticSizing{
		inboundRate:     staticHeadroom * steady[n.Name],
		edgeMaxInflight: make([]int, len(n.Edges)),
		edgeCBConfig:    make([]benchmarks.StaticCBConfig, len(n.Edges)),
	}
	for e, edge := range n.Edges {
		edgeRPS := edge.Prob * steady[n.Name]
		subtreeS := topo.SubtreeMeanLatencyMS(edge.Callee, defaultHopMS) / 1000.0
		maxInflight := int(math.Ceil(staticHeadroom * edgeRPS * subtreeS / float64(replicas)))
		if maxInflight < 2 {
			maxInflight = 2
		}
		s.edgeMaxInflight[e] = maxInflight
		s.edgeCBConfig[e] = benchmarks.StaticBAUConfig
		if edgeRPS/float64(replicas) > peakConfigMinRPS {
			s.edgeCBConfig[e] = benchmarks.StaticPeakConfig
		}
	}
	return s
}

func newStaticGovernor(nodeIdx int, topo *Topology, steady map[string]float64) *staticGovernor {
	n := topo.Nodes[nodeIdx]
	sizing := staticNodeSizing(topo, steady, nodeIdx)
	g := &staticGovernor{
		spec:      n,
		sizing:    sizing,
		bucket:    NewTokenBucket(sizing.inboundRate, sizing.inboundRate),
		inTracker: &RoleTracker{Name: "in:" + n.Name},
	}
	g.all = append(g.all, g.inTracker)
	g.Resize(n.MinReplicas, 0)
	return g
}

// SurgeSteadyRPS returns the steady-state map with an entry node's rate
// scaled, for sizing the Peak static variant against the surge phase.
func SurgeSteadyRPS(topo *Topology, entryName string, mult float64) (map[string]float64, error) {
	scaled := make([]NodeSpec, len(topo.Nodes))
	copy(scaled, topo.Nodes)
	for i := range scaled {
		if scaled[i].Name == entryName {
			scaled[i].EntryRPS *= mult
		}
	}
	return NewTopology(scaled).SteadyStateRPS()
}
