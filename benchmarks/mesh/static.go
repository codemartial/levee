package mesh

import (
	"math"
	"time"

	"github.com/codemartial/levee/benchmarks"
)

// Static sizing derivations. The operator's only load expectations are the
// provisioning profile itself -- initial scale and max capacity -- plus the
// safety margins they would bake in. No scenario foreknowledge: no entry
// rates, no surge multipliers, no solved steady-state traffic.
//
// Inbound token bucket, one per replica (local rate limiting, the standard
// per-pod deployment):
//   rate = healthyCapFrac * PerReplicaRPS
// Each replica admits what it can serve inside its healthy envelope (the
// same 85% ceiling Topology.Validate enforces), so aggregate admission
// tracks the live replica count: it protects the queue while the
// autoscaler lags and widens as capacity actually arrives, up to
// healthyCapFrac * MaxReplicas * PerReplicaRPS at full scale. Demand above
// that is unservable no matter what arrives, so the limit derives from
// capacity, not from a load forecast. 85% rather than the autoscaler's 70%
// target keeps admitted utilization able to exceed target, so throttled
// surges still generate scale-up signals. burst = 1 second of tokens,
// absorbing Poisson micro-bursts without admitting sustained overload.
//
// Per-replica outbound concurrency limiter on edge N->M, via Little's Law:
//   mean = p_edge * PerReplicaRPS * subtreeMeanLatencyS(M)
//   perInstanceMaxInflight = ceil(max(headroom * mean, mean + 3*sqrt(mean)))
// A replica cannot drive an edge faster than its own service ceiling
// PerReplicaRPS, so p_edge * PerReplicaRPS bounds the per-instance edge rate
// and the subtree mean is the expected end-to-end latency of a call into M
// (local mean plus deepest probability-weighted sync branch, see
// Topology.SubtreeMeanLatencyMS). At healthy latency the cap sits well clear
// of normal inflight; it binds when callee latency inflates or calls per
// request multiply. The 3-sigma term covers Poisson burstiness, which
// dominates at the small per-instance means these limits take. Floor of 2 so
// probing is never single-file.
//
// Per-replica outbound breaker config depends on what else is deployed.
// Standalone (breaker-only), the breaker is the sole protection, so it must
// also do congestion duty: configs follow the documented derivation ranges
// in static_cb.go by the per-instance volume each breaker can see, bounded
// by the replica's service ceiling -- BAU for <= 800 RPS, Peak above. At
// this topology's per-replica ceilings (<= 200 RPS) every edge lands on
// BAU.
//
// Chained behind the concurrency limiter, the breaker's job narrows to the
// one case the limiter cannot express: a dependency that is down rather
// than slow. Tripping a sync edge fails the parent request outright, while
// serving it at success rate s still gives the parent chance s, so a
// chained sync-edge breaker pays off only when s is near zero. Sizing from
// E[n] = 1/(p^T * (1-p)), calls until T consecutive failures at failure
// rate p: at T=20, any partial degradation (p <= 0.5) needs >= 2.1M calls
// to trip (never, at per-replica volumes), while a dead edge (p >= 0.9)
// trips within ~20-80 calls, i.e. seconds. Async edges keep the aggressive
// BAU config even when chained: a dropped callback never fails its parent,
// so false trips only shed optional work.
const (
	staticHeadroom   = 1.5
	healthyCapFrac   = 0.85
	peakConfigMinRPS = 800.0
)

// deadEdgeCBConfig is the chained-stack sync-edge breaker: inert at any
// partial failure rate, cuts an effectively dead edge within seconds.
var deadEdgeCBConfig = benchmarks.StaticCBConfig{
	FailureThreshold: 20,
	SuccessThreshold: 3,
	HalfOpenTimeout:  10 * time.Second,
	HalfOpenMaxCalls: 1,
}

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

// staticNodeSizing derives all static limits for one node from its
// provisioning profile. Breaker configs depend on parts: chained behind
// limiters, sync-edge breakers narrow to dead-edge detection.
func staticNodeSizing(topo *Topology, nodeIdx int, parts StaticParts) staticSizing {
	n := topo.Nodes[nodeIdx]
	chained := parts&StaticBreaker != 0 && parts&StaticLimiter != 0
	s := staticSizing{
		inboundRate:     healthyCapFrac * float64(n.PerReplicaRPS),
		edgeMaxInflight: make([]int, len(n.Edges)),
		edgeCBConfig:    make([]benchmarks.StaticCBConfig, len(n.Edges)),
	}
	for e, edge := range n.Edges {
		edgeRPS := edge.Prob * float64(n.PerReplicaRPS)
		subtreeS := topo.SubtreeMeanLatencyMS(edge.Callee, defaultHopMS) / 1000.0
		mean := edgeRPS * subtreeS
		maxInflight := int(math.Ceil(math.Max(staticHeadroom*mean, mean+3*math.Sqrt(mean))))
		if maxInflight < 2 {
			maxInflight = 2
		}
		s.edgeMaxInflight[e] = maxInflight
		switch {
		case chained && !edge.Async:
			s.edgeCBConfig[e] = deadEdgeCBConfig
		case edgeRPS > peakConfigMinRPS:
			s.edgeCBConfig[e] = benchmarks.StaticPeakConfig
		default:
			s.edgeCBConfig[e] = benchmarks.StaticBAUConfig
		}
	}
	return s
}

func newStaticGovernor(nodeIdx int, topo *Topology, parts StaticParts) *staticGovernor {
	n := topo.Nodes[nodeIdx]
	g := &staticGovernor{
		spec:   n,
		sizing: staticNodeSizing(topo, nodeIdx, parts),
		parts:  parts,
	}
	g.Resize(n.MinReplicas, 0)
	return g
}
