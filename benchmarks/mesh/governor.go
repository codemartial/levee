package mesh

import (
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
)

// State duration buckets for RoleTracker.
const (
	bucketClosed = iota
	bucketThrottled
	bucketOpen // OPEN and HALF_OPEN combined
	numBuckets
)

// Governor is one node's admission composition. Per-replica instances scale
// with the node (Resize); InboundStart picks one round-robin and returns its
// index, which routes that request's outcome and outbound calls.
type Governor interface {
	InboundStart(ts time.Time) (rep int, err error)
	InboundDone(rep int, ts time.Time, d time.Duration, ok bool)
	OutboundStart(rep, edge int, ts time.Time) error
	OutboundDone(rep, edge int, ts time.Time, d time.Duration, ok bool)
	Resize(replicas int, tsNS int64)
	Roles() []*RoleTracker
}

// Candidate builds a Governor for each node of a topology.
type Candidate struct {
	Name string
	New  func(nodeIdx int, topo *Topology, slo levee.SLO) Governor
}

// RoleTracker accumulates time-in-state for one governor role instance.
// StateChange.State is just the post-call state (Trigger is always nil), so
// transitions are derived by comparing against the previously seen state.
type RoleTracker struct {
	Name        string
	DurNS       [numBuckets]int64
	Transitions int
	cur         int
	lastNS      int64
	started     bool
	retired     bool
}

func stateBucket(s levee.State) int {
	switch s {
	case levee.THROTTLED:
		return bucketThrottled
	case levee.OPEN, levee.HALF_OPEN:
		return bucketOpen
	default:
		return bucketClosed
	}
}

// Observe attributes elapsed time to the previous state and switches to b.
func (rt *RoleTracker) Observe(b int, tsNS int64) {
	if rt.retired {
		return
	}
	if !rt.started {
		rt.started = true
		rt.cur = b
		rt.lastNS = tsNS
		return
	}
	if tsNS > rt.lastNS {
		rt.DurNS[rt.cur] += tsNS - rt.lastNS
		rt.lastNS = tsNS
	}
	if b != rt.cur {
		rt.Transitions++
		rt.cur = b
	}
}

// Flush closes the final interval; retired trackers are already closed.
func (rt *RoleTracker) Flush(endNS int64) {
	if rt.retired {
		return
	}
	if !rt.started {
		rt.DurNS[bucketClosed] += endNS
		return
	}
	if endNS > rt.lastNS {
		rt.DurNS[rt.cur] += endNS - rt.lastNS
		rt.lastNS = endNS
	}
}

// retire closes the tracker when its instance is torn down (scale-down/crash).
func (rt *RoleTracker) retire(tsNS int64) {
	rt.Flush(tsNS)
	rt.retired = true
}

// leveeInstance is one replica's process-local levee set.
type leveeInstance struct {
	inbound  *levee.Levee
	outbound []*levee.Levee
	trackers []*RoleTracker // 0 = inbound, 1+e = edge e
}

// leveeGovernor: per replica, one levee inbound plus one per outbound edge.
type leveeGovernor struct {
	spec      NodeSpec
	slo       levee.SLO
	instances []*leveeInstance
	all       []*RoleTracker
	rr        int
}

// NewLeveeCandidate builds the levee mesh candidate.
func NewLeveeCandidate() Candidate {
	return Candidate{
		Name: "Levee",
		New: func(nodeIdx int, topo *Topology, slo levee.SLO) Governor {
			g := &leveeGovernor{spec: topo.Nodes[nodeIdx], slo: slo}
			g.Resize(topo.Nodes[nodeIdx].MinReplicas, 0)
			return g
		},
	}
}

func (g *leveeGovernor) Resize(replicas int, tsNS int64) {
	for len(g.instances) > replicas {
		last := g.instances[len(g.instances)-1]
		for _, rt := range last.trackers {
			rt.retire(tsNS)
		}
		g.instances = g.instances[:len(g.instances)-1]
	}
	for len(g.instances) < replicas {
		inst := &leveeInstance{
			inbound:  levee.NewLevee(g.slo),
			outbound: make([]*levee.Levee, len(g.spec.Edges)),
			trackers: make([]*RoleTracker, 1+len(g.spec.Edges)),
		}
		inst.trackers[0] = &RoleTracker{Name: "in:" + g.spec.Name}
		for e, edge := range g.spec.Edges {
			inst.outbound[e] = levee.NewLevee(g.slo)
			inst.trackers[1+e] = &RoleTracker{Name: "out:" + g.spec.Name + "->" + edge.Callee}
		}
		g.all = append(g.all, inst.trackers...)
		g.instances = append(g.instances, inst)
	}
	if g.rr >= len(g.instances) {
		g.rr = 0
	}
}

func (g *leveeGovernor) InboundStart(ts time.Time) (int, error) {
	if len(g.instances) == 0 {
		return -1, levee.ErrCircuitOpen
	}
	rep := g.rr
	g.rr = (g.rr + 1) % len(g.instances)
	inst := g.instances[rep]
	sc, err := inst.inbound.Start(ts)
	inst.trackers[0].Observe(stateBucket(sc.State), ts.UnixNano())
	return rep, err
}

func (g *leveeGovernor) InboundDone(rep int, ts time.Time, d time.Duration, ok bool) {
	if rep < 0 || rep >= len(g.instances) {
		return
	}
	inst := g.instances[rep]
	var sc levee.StateChange
	if ok {
		sc = inst.inbound.Success(ts, d)
	} else {
		sc = inst.inbound.Fail(ts, d)
	}
	inst.trackers[0].Observe(stateBucket(sc.State), ts.UnixNano())
}

func (g *leveeGovernor) OutboundStart(rep, edge int, ts time.Time) error {
	if rep < 0 || rep >= len(g.instances) {
		return levee.ErrCircuitOpen
	}
	inst := g.instances[rep]
	sc, err := inst.outbound[edge].Start(ts)
	inst.trackers[1+edge].Observe(stateBucket(sc.State), ts.UnixNano())
	return err
}

func (g *leveeGovernor) OutboundDone(rep, edge int, ts time.Time, d time.Duration, ok bool) {
	if rep < 0 || rep >= len(g.instances) {
		return
	}
	inst := g.instances[rep]
	var sc levee.StateChange
	if ok {
		sc = inst.outbound[edge].Success(ts, d)
	} else {
		sc = inst.outbound[edge].Fail(ts, d)
	}
	inst.trackers[1+edge].Observe(stateBucket(sc.State), ts.UnixNano())
}

func (g *leveeGovernor) Roles() []*RoleTracker { return g.all }

// StaticParts selects which mechanisms a static candidate deploys.
type StaticParts uint8

const (
	// StaticBreaker: per-replica circuit breakers on outbound edges.
	StaticBreaker StaticParts = 1 << iota
	// StaticLimiter: node-level inbound token bucket plus per-replica
	// max-inflight limiters on outbound edges (outcome-blind flow control).
	StaticLimiter
)

// staticInstance is one replica's process-local bucket + breaker + limiter
// set. Fields are nil for parts the candidate does not deploy.
type staticInstance struct {
	inBucket *TokenBucket
	outCB    []*benchmarks.StaticCB
	outLim   []*ConcurrencyLimiter
	trackers []*RoleTracker // 0 = inbound, 1+e = edge e
}

// staticGovernor: per replica, an inbound token bucket (local rate
// limiting) plus breaker + concurrency-limiter instances per outbound edge.
type staticGovernor struct {
	spec      NodeSpec
	sizing    staticSizing
	parts     StaticParts
	instances []*staticInstance
	all       []*RoleTracker
	rr        int
}

// NewStaticCandidate builds a static candidate deploying the given parts,
// sized from each node's provisioning profile; see static.go for the
// sizing derivations.
func NewStaticCandidate(name string, parts StaticParts) Candidate {
	return Candidate{
		Name: name,
		New: func(nodeIdx int, topo *Topology, slo levee.SLO) Governor {
			return newStaticGovernor(nodeIdx, topo, parts)
		},
	}
}

func (g *staticGovernor) Resize(replicas int, tsNS int64) {
	for len(g.instances) > replicas {
		last := g.instances[len(g.instances)-1]
		for _, rt := range last.trackers {
			rt.retire(tsNS)
		}
		g.instances = g.instances[:len(g.instances)-1]
	}
	for len(g.instances) < replicas {
		inst := &staticInstance{
			trackers: make([]*RoleTracker, 1+len(g.spec.Edges)),
		}
		inst.trackers[0] = &RoleTracker{Name: "in:" + g.spec.Name}
		if g.parts&StaticBreaker != 0 {
			inst.outCB = make([]*benchmarks.StaticCB, len(g.spec.Edges))
		}
		if g.parts&StaticLimiter != 0 {
			inst.inBucket = NewTokenBucket(g.sizing.inboundRate, g.sizing.inboundRate)
			inst.outLim = make([]*ConcurrencyLimiter, len(g.spec.Edges))
		}
		for e, edge := range g.spec.Edges {
			if inst.outCB != nil {
				inst.outCB[e] = benchmarks.NewStaticCB(g.sizing.edgeCBConfig[e])
			}
			if inst.outLim != nil {
				inst.outLim[e] = NewConcurrencyLimiter(g.sizing.edgeMaxInflight[e])
			}
			inst.trackers[1+e] = &RoleTracker{Name: "out:" + g.spec.Name + "->" + edge.Callee}
		}
		g.all = append(g.all, inst.trackers...)
		g.instances = append(g.instances, inst)
	}
	if g.rr >= len(g.instances) {
		g.rr = 0
	}
}

func (g *staticGovernor) InboundStart(ts time.Time) (int, error) {
	if len(g.instances) == 0 {
		return -1, levee.ErrCircuitOpen
	}
	rep := g.rr
	g.rr = (g.rr + 1) % len(g.instances)
	inst := g.instances[rep]
	tsNS := ts.UnixNano()
	if inst.inBucket == nil {
		inst.trackers[0].Observe(bucketClosed, tsNS)
		return rep, nil
	}
	ok := inst.inBucket.Allow(tsNS)
	g.observeInbound(inst, tsNS)
	if !ok {
		return rep, levee.ErrCircuitOpen
	}
	return rep, nil
}

func (g *staticGovernor) InboundDone(rep int, ts time.Time, d time.Duration, ok bool) {
	if rep < 0 || rep >= len(g.instances) {
		return
	}
	g.observeInbound(g.instances[rep], ts.UnixNano())
}

// observeInbound reports THROTTLED while the bucket cannot admit a request.
func (g *staticGovernor) observeInbound(inst *staticInstance, tsNS int64) {
	b := bucketClosed
	if inst.inBucket != nil && inst.inBucket.Saturated(tsNS) {
		b = bucketThrottled
	}
	inst.trackers[0].Observe(b, tsNS)
}

// OutboundStart consults the breaker first so it can advance its OPEN ->
// HALF_OPEN clock; a subsequent limiter rejection cancels the admission so
// half-open probe slots are never leaked or consumed by limiter pressure.
func (g *staticGovernor) OutboundStart(rep, edge int, ts time.Time) error {
	if rep < 0 || rep >= len(g.instances) {
		return levee.ErrCircuitOpen
	}
	inst := g.instances[rep]
	tsNS := ts.UnixNano()
	if inst.outCB != nil {
		if _, err := inst.outCB[edge].Start(ts); err != nil {
			g.observeOutbound(inst, edge, tsNS)
			return err
		}
	}
	if inst.outLim != nil && !inst.outLim[edge].TryAcquire() {
		if inst.outCB != nil {
			inst.outCB[edge].Cancel()
		}
		g.observeOutbound(inst, edge, tsNS)
		return levee.ErrCircuitOpen
	}
	g.observeOutbound(inst, edge, tsNS)
	return nil
}

func (g *staticGovernor) OutboundDone(rep, edge int, ts time.Time, d time.Duration, ok bool) {
	if rep < 0 || rep >= len(g.instances) {
		return
	}
	inst := g.instances[rep]
	if inst.outLim != nil {
		inst.outLim[edge].Release()
	}
	if inst.outCB != nil {
		if ok {
			inst.outCB[edge].Success(ts, d)
		} else {
			inst.outCB[edge].Fail(ts, d)
		}
	}
	g.observeOutbound(inst, edge, ts.UnixNano())
}

// observeOutbound precedence: breaker OPEN, else limiter saturation, else CLOSED.
func (g *staticGovernor) observeOutbound(inst *staticInstance, edge int, tsNS int64) {
	b := bucketClosed
	if inst.outCB != nil && inst.outCB[edge].State() == levee.OPEN {
		b = bucketOpen
	} else if inst.outLim != nil && inst.outLim[edge].Saturated() {
		b = bucketThrottled
	}
	inst.trackers[1+edge].Observe(b, tsNS)
}

func (g *staticGovernor) Roles() []*RoleTracker { return g.all }

// noGovernor admits everything; the negative control.
type noGovernor struct {
	trackers []*RoleTracker
}

// NewNoGovCandidate builds the no-governor control candidate.
func NewNoGovCandidate() Candidate {
	return Candidate{
		Name: "No-Gov",
		New: func(nodeIdx int, topo *Topology, slo levee.SLO) Governor {
			return &noGovernor{trackers: []*RoleTracker{{Name: "in:" + topo.Nodes[nodeIdx].Name}}}
		},
	}
}

func (g *noGovernor) InboundStart(ts time.Time) (int, error) {
	g.trackers[0].Observe(bucketClosed, ts.UnixNano())
	return 0, nil
}
func (g *noGovernor) InboundDone(rep int, ts time.Time, d time.Duration, ok bool)            {}
func (g *noGovernor) OutboundStart(rep, edge int, ts time.Time) error                        { return nil }
func (g *noGovernor) OutboundDone(rep, edge int, ts time.Time, d time.Duration, ok bool)     {}
func (g *noGovernor) Resize(replicas int, tsNS int64)                                        {}
func (g *noGovernor) Roles() []*RoleTracker                                                  { return g.trackers }
