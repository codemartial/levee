package mesh

import (
	"container/heap"
	"math/rand/v2"
	"time"

	"github.com/codemartial/levee"
)

// Simulation constants shared across the mesh benchmark.
const (
	DefaultSeed     = uint64(20241225)
	bufferSeconds   = 3 // queue limit = 3 seconds' worth of instantaneous capacity
	defaultHopMS    = 1.0
	hopNS           = int64(1e6)
	rootTimeoutNS   = int64(1500 * 1e6)
	rootHopBudget   = 8
	tickIntervalNS  = int64(1e9)
	crashRecoveryNS = int64(120 * 1e9)
	baselineErrRate = 0.005
)

// Hash salts for per-call candidate-independent draws.
const (
	saltLatency  = uint64(0xA24BAED4963EE407)
	saltError    = uint64(0x9FB21C651E98DF25)
	saltEdgeBase = uint64(0xD6E8FEB86659FD93)
	saltEdgeStep = uint64(0x2545F4914F6CDD1D)
	saltChild    = uint64(0x9E3779B97F4A7C15)
	saltEdgeRep  = uint64(0xC2B2AE3D27D4EB4F) // per-repetition salt for amplified edges
)

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// drawFloat maps a stable call ID and salt to a uniform draw in [0, 1).
// Keyed draws stay identical across candidates regardless of admission decisions.
func drawFloat(id, salt uint64) float64 {
	return float64(splitmix64(id^salt)>>11) / (1 << 53)
}

type eventKind uint8

const (
	evArrival eventKind = iota
	evCallArrive
	evNodeComplete
	evRootTimeout
	evCrashRecover
	evCapacityTick
)

// event is one heap entry; (atNS, seq) gives a strict deterministic order.
type event struct {
	atNS int64
	seq  uint64
	kind eventKind
	node int // entry slot for evArrival; node index for tick/recover
	c    *call
	gen  uint64 // node generation for evNodeComplete staleness
}

type eventHeap []*event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].atNS != h[j].atNS {
		return h[i].atNS < h[j].atNS
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// call is one hop of one request tree. Logical outcome (finished/ok) is
// decoupled from physical occupancy, which drains only at node completion.
type call struct {
	id              uint64
	node            int
	parent          *call
	edgeIdx         int // index into parent's edges
	root            *call
	deadlineNS      int64
	hopBudget       int
	entry           int // entry slot for entry roots, else -1
	asyncCallerNode int // caller node for detached async roots, else -1
	asyncCallerEdge int
	asyncCallerRep  int
	asyncCallerGen  uint64
	dispatchNS      int64
	admitNS         int64
	admitGen        uint64
	govRep          int // admitting governor replica; routes outcome + outbound
	admitted        bool
	localDone       bool
	localOK         bool
	childFailed     bool
	pendingChildren int
	finished        bool
}

// phaseFx is a phase precompiled to per-slot/per-node multiplier arrays.
type phaseFx struct {
	startNS, endNS int64
	rampNS         int64       // linear arrival ramp-in from 1.0 to rate[slot]
	rate           []float64   // per entry slot
	lat            []float64   // per node
	err            []float64   // per node
	edge           [][]float64 // per node, per edge index: calls-per-request multiplier
}

// Engine is a single-threaded discrete-event simulation of one candidate
// governor composition over the topology. Not safe for concurrent use.
type Engine struct {
	topo       *Topology
	slo        levee.SLO
	seed       uint64
	nodes      []*nodeRuntime
	entries    []int // node index per entry slot
	entryRNG   []*rand.Rand
	arrivalSeq []uint64
	fx         []phaseFx
	events     eventHeap
	seq        uint64
	endNS      int64
	liveByRoot map[*call][]*call
	metrics    *MeshMetrics
}

// NewEngine wires governors, entry streams, and precompiled phases.
func NewEngine(topo *Topology, cand Candidate, phases []Phase, slo levee.SLO, seed uint64) *Engine {
	e := &Engine{
		topo:       topo,
		slo:        slo,
		seed:       seed,
		nodes:      make([]*nodeRuntime, len(topo.Nodes)),
		liveByRoot: make(map[*call][]*call),
	}
	for i, spec := range topo.Nodes {
		e.nodes[i] = newNodeRuntime(i, spec, cand.New(i, topo, slo))
		if spec.EntryRPS > 0 {
			slot := len(e.entries)
			e.entries = append(e.entries, i)
			e.entryRNG = append(e.entryRNG, rand.New(rand.NewPCG(seed+uint64(slot)*7919, (seed>>32)+uint64(slot)+1)))
		}
	}
	// Nodes and governor instances begin at design replicas (warm start).
	// Skipped for test topologies whose steady state does not converge.
	if steady, err := topo.SteadyStateRPS(); err == nil {
		for i, spec := range topo.Nodes {
			e.nodes[i].cc.WarmStart(designReplicas(spec, steady[spec.Name]), steady[spec.Name], time.Unix(0, 0))
			e.nodes[i].resize(0)
			e.nodes[i].syncGov(0)
		}
	}
	e.arrivalSeq = make([]uint64, len(e.entries))
	e.fx = make([]phaseFx, len(phases))
	for p, ph := range phases {
		fx := phaseFx{
			startNS: int64(ph.StartS) * 1e9,
			endNS:   int64(ph.EndS) * 1e9,
			rampNS:  int64(ph.RampS) * 1e9,
			rate:    make([]float64, len(e.entries)),
			lat:     make([]float64, len(topo.Nodes)),
			err:     make([]float64, len(topo.Nodes)),
		}
		for s, ni := range e.entries {
			fx.rate[s] = 1.0
			if m, ok := ph.EntryRateMult[topo.Nodes[ni].Name]; ok {
				fx.rate[s] = m
			}
		}
		fx.edge = make([][]float64, len(topo.Nodes))
		matched := 0
		for i, n := range topo.Nodes {
			fx.lat[i] = 1.0
			if m, ok := ph.LatencyMult[n.Name]; ok {
				fx.lat[i] = m
			}
			fx.err[i] = ph.ExtraErrorRate[n.Name]
			fx.edge[i] = make([]float64, len(n.Edges))
			for j, edge := range n.Edges {
				fx.edge[i][j] = 1.0
				if m, ok := ph.EdgeCallMult[n.Name+"->"+edge.Callee]; ok {
					fx.edge[i][j] = m
					matched++
				}
			}
		}
		if matched != len(ph.EdgeCallMult) {
			panic("phase " + ph.Name + ": EdgeCallMult key does not match any topology edge")
		}
		e.fx[p] = fx
	}
	e.metrics = newMeshMetrics(e)
	return e
}

func (e *Engine) rateMultAt(slot int, tNS int64) float64 {
	for i := range e.fx {
		if tNS >= e.fx[i].startNS && tNS < e.fx[i].endNS {
			m := e.fx[i].rate[slot]
			if r := e.fx[i].rampNS; r > 0 && tNS < e.fx[i].startNS+r {
				m = 1 + (m-1)*float64(tNS-e.fx[i].startNS)/float64(r)
			}
			return m
		}
	}
	return 1.0
}

func (e *Engine) latMultAt(node int, tNS int64) float64 {
	for i := range e.fx {
		if tNS >= e.fx[i].startNS && tNS < e.fx[i].endNS {
			return e.fx[i].lat[node]
		}
	}
	return 1.0
}

func (e *Engine) extraErrAt(node int, tNS int64) float64 {
	for i := range e.fx {
		if tNS >= e.fx[i].startNS && tNS < e.fx[i].endNS {
			return e.fx[i].err[node]
		}
	}
	return 0.0
}

func (e *Engine) edgeMultAt(node, edgeIdx int, tNS int64) float64 {
	for i := range e.fx {
		if tNS >= e.fx[i].startNS && tNS < e.fx[i].endNS {
			return e.fx[i].edge[node][edgeIdx]
		}
	}
	return 1.0
}

func (e *Engine) push(ev *event) {
	ev.seq = e.seq
	e.seq++
	heap.Push(&e.events, ev)
}

// Run simulates [0, endS) of synthetic time plus a drain tail, then
// finalizes and returns the metrics.
func (e *Engine) Run(endS int) *MeshMetrics {
	e.endNS = int64(endS) * 1e9
	for slot := range e.entries {
		e.scheduleArrival(slot, 0)
	}
	for i := range e.nodes {
		e.push(&event{atNS: tickIntervalNS, kind: evCapacityTick, node: i})
	}
	for len(e.events) > 0 {
		ev := heap.Pop(&e.events).(*event)
		t := ev.atNS
		switch ev.kind {
		case evArrival:
			e.handleArrival(ev.node, t)
		case evCallArrive:
			e.deliver(ev.c, t)
		case evNodeComplete:
			e.handleComplete(ev.c, ev.gen, t)
		case evRootTimeout:
			if !ev.c.finished {
				e.failTree(ev.c, t)
			}
		case evCrashRecover:
			n := e.nodes[ev.node]
			since := n.crashedSinceNS
			n.recover(t)
			e.metrics.addDowntime(ev.node, since, t, e.endNS)
		case evCapacityTick:
			n := e.nodes[ev.node]
			if !n.crashed {
				n.cc.Tick(time.Unix(0, t))
				n.resize(t)
				n.syncGov(t)
			}
			if t < e.endNS+rootTimeoutNS {
				e.push(&event{atNS: t + tickIntervalNS, kind: evCapacityTick, node: ev.node})
			}
		}
	}
	e.finalize()
	return e.metrics
}

// scheduleArrival draws the next Poisson arrival for an entry slot.
// The rate is sampled at generation time; phase boundaries are minute-scale
// so the boundary-crossing error is negligible.
func (e *Engine) scheduleArrival(slot int, fromNS int64) {
	rps := e.topo.Nodes[e.entries[slot]].EntryRPS * e.rateMultAt(slot, fromNS)
	waitNS := int64(e.entryRNG[slot].ExpFloat64() / rps * 1e9)
	if waitNS < 1 {
		waitNS = 1
	}
	at := fromNS + waitNS
	if at < e.endNS {
		e.push(&event{atNS: at, kind: evArrival, node: slot})
	}
}

func (e *Engine) handleArrival(slot int, t int64) {
	e.scheduleArrival(slot, t)
	e.arrivalSeq[slot]++
	id := splitmix64(splitmix64(e.seed+uint64(slot)*saltChild) + e.arrivalSeq[slot])
	root := &call{
		id:              id,
		node:            e.entries[slot],
		deadlineNS:      t + rootTimeoutNS,
		hopBudget:       rootHopBudget,
		entry:           slot,
		asyncCallerNode: -1,
	}
	root.root = root
	e.push(&event{atNS: root.deadlineNS, kind: evRootTimeout, c: root})
	e.deliver(root, t)
}

// deliver runs inbound admission at the call's node.
func (e *Engine) deliver(c *call, t int64) {
	if t >= c.deadlineNS {
		e.finishLogical(c, false, t)
		return
	}
	n := e.nodes[c.node]
	e.liveByRoot[c.root] = append(e.liveByRoot[c.root], c)
	if n.crashed {
		// Parked; upstream observes only the eventual timeout. A crashed
		// node performs no admission, so entry roots count as allowed.
		if c == c.root && c.entry >= 0 {
			e.metrics.recordAllowed(c.entry, 0)
		}
		return
	}
	rep, err := n.gov.InboundStart(time.Unix(0, t))
	if err != nil {
		if c == c.root && c.entry >= 0 {
			// Entry-blocked roots count as Blocked, not as scored failures,
			// matching the distributed suite's semantics.
			e.metrics.recordBlocked(c.entry)
			c.finished = true
			delete(e.liveByRoot, c)
			return
		}
		e.finishLogical(c, false, t)
		return
	}
	c.admitted = true
	c.admitNS = t
	c.admitGen = n.gen
	c.govRep = rep
	if c == c.root && c.entry >= 0 {
		e.metrics.recordAllowed(c.entry, int64(n.backlog+1))
	}
	n.cc.RecordRequest(time.Unix(0, t))
	n.backlog++
	if n.backlog >= n.bufferLimit() {
		n.crash(t)
		e.push(&event{atNS: t + crashRecoveryNS, kind: evCrashRecover, node: c.node})
		return
	}
	serviceNS := int64(n.profile.sample(drawFloat(c.id, saltLatency)) * e.latMultAt(c.node, t) * 1e6)
	if serviceNS < 1 {
		serviceNS = 1
	}
	e.push(&event{atNS: n.admit(t, serviceNS), kind: evNodeComplete, c: c, gen: n.gen})
}

// handleComplete releases physical occupancy; logically dead calls only
// consumed capacity (work amplification), nothing more happens for them.
func (e *Engine) handleComplete(c *call, gen uint64, t int64) {
	n := e.nodes[c.node]
	if n.crashed || gen != n.gen {
		return // work lost in a crash; occupancy was reset there
	}
	n.backlog--
	if c.finished {
		return
	}
	c.localDone = true
	c.localOK = drawFloat(c.id, saltError) >= baselineErrRate+e.extraErrAt(c.node, t)
	e.dispatchChildren(c, t)
	if c.pendingChildren == 0 {
		e.finishLogical(c, c.localOK && !c.childFailed, t)
	}
}

// dispatchChildren issues downstream calls at local completion time, so
// parent latency = local latency + hop + max of sync child subtree latencies.
func (e *Engine) dispatchChildren(c *call, t int64) {
	spec := e.topo.Nodes[c.node]
	gov := e.nodes[c.node].gov
	for i, edge := range spec.Edges {
		// Expected calls per request on this edge; above 1.0 only under an
		// EdgeCallMult injection. Repetition 0 matches the un-amplified draw.
		expected := edge.Prob * e.edgeMultAt(c.node, i, t)
		for k := 0; float64(k) < expected; k++ {
			p := expected - float64(k)
			if p < 1 && drawFloat(c.id, saltEdgeBase+uint64(i)*saltEdgeStep+uint64(k)*saltEdgeRep) >= p {
				continue
			}
			if c.hopBudget <= 1 {
				if !edge.Async {
					c.childFailed = true
				}
				continue
			}
			childID := splitmix64(c.id + uint64(i+1)*saltChild + uint64(k)*saltEdgeRep)
			callee := e.topo.index[edge.Callee]
			ts := time.Unix(0, t)
			if edge.Async {
				if gov.OutboundStart(c.govRep, i, ts) != nil {
					continue // dropped callback; parent unaffected
				}
				a := &call{
					id:              childID,
					node:            callee,
					deadlineNS:      t + rootTimeoutNS,
					hopBudget:       c.hopBudget - 1,
					entry:           -1,
					asyncCallerNode: c.node,
					asyncCallerEdge: i,
					asyncCallerRep:  c.govRep,
					asyncCallerGen:  c.admitGen,
					dispatchNS:      t,
				}
				a.root = a
				e.push(&event{atNS: a.deadlineNS, kind: evRootTimeout, c: a})
				e.push(&event{atNS: t + hopNS, kind: evCallArrive, c: a})
			} else {
				if gov.OutboundStart(c.govRep, i, ts) != nil {
					c.childFailed = true // local rejection = that call's failure
					continue
				}
				ch := &call{
					id:              childID,
					node:            callee,
					parent:          c,
					edgeIdx:         i,
					root:            c.root,
					deadlineNS:      c.deadlineNS,
					hopBudget:       c.hopBudget - 1,
					entry:           -1,
					asyncCallerNode: -1,
					dispatchNS:      t,
				}
				c.pendingChildren++
				e.push(&event{atNS: t + hopNS, kind: evCallArrive, c: ch})
			}
		}
	}
}

// finishLogical resolves a call's outcome exactly once, reporting to the
// node's inbound governor and the caller's edge governor. Never touches
// physical occupancy.
func (e *Engine) finishLogical(c *call, ok bool, t int64) {
	if c.finished {
		return
	}
	c.finished = true
	n := e.nodes[c.node]
	// Skip InboundDone if the node crashed after admission: the instance
	// died with the node and nobody observed a response.
	if c.admitted && c.admitGen == n.gen {
		n.gov.InboundDone(c.govRep, time.Unix(0, t), time.Duration(t-c.admitNS), ok)
	}
	if c.parent != nil {
		p := c.parent
		// The caller-side edge instance also dies if the caller crashed.
		if e.nodes[p.node].gen == p.admitGen {
			e.nodes[p.node].gov.OutboundDone(p.govRep, c.edgeIdx, time.Unix(0, t), time.Duration(t-c.dispatchNS), ok)
		}
		if !ok {
			p.childFailed = true
		}
		p.pendingChildren--
		if !p.finished && p.localDone && p.pendingChildren == 0 {
			e.finishLogical(p, p.localOK && !p.childFailed, t)
		}
		return
	}
	if c.asyncCallerNode >= 0 {
		if e.nodes[c.asyncCallerNode].gen == c.asyncCallerGen {
			e.nodes[c.asyncCallerNode].gov.OutboundDone(c.asyncCallerRep, c.asyncCallerEdge, time.Unix(0, t), time.Duration(t-c.dispatchNS), ok)
		}
	} else if c.entry >= 0 {
		e.metrics.recordResult(c.entry, t, ok)
	}
	delete(e.liveByRoot, c)
}

// failTree resolves every live call of a timed-out tree as a failure,
// children before parents. Physical work keeps its slots.
func (e *Engine) failTree(root *call, t int64) {
	list := e.liveByRoot[root]
	for i := len(list) - 1; i >= 0; i-- {
		e.finishLogical(list[i], false, t)
	}
	if !root.finished {
		e.finishLogical(root, false, t)
	}
	delete(e.liveByRoot, root)
}

// finalize closes open crash windows at sim end.
func (e *Engine) finalize() {
	for i, n := range e.nodes {
		if n.crashed {
			e.metrics.addDowntime(i, n.crashedSinceNS, e.endNS, e.endNS)
		}
	}
	e.metrics.finalize(e.endNS)
}
