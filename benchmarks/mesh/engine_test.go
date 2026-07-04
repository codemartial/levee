package mesh

import (
	"testing"
	"time"

	"github.com/codemartial/levee"
)

var testSLO = levee.SLO{SuccessRate: 0.90, Timeout: 1500 * time.Millisecond}

// spyEvent records one governor interaction for white-box assertions.
type spyEvent struct {
	node string
	kind string // in-start, in-done, out-start, out-done
	rep  int
	edge int
	tsNS int64
	ok   bool
	dur  time.Duration
}

// spyGovernor admits everything (like noGovernor), scales instance count
// with replicas, round-robins, and records every call.
type spyGovernor struct {
	node     string
	rec      *[]spyEvent
	replicas int
	rr       int
	trackers []*RoleTracker
}

func newSpyCandidate(rec *[]spyEvent) Candidate {
	return Candidate{
		Name: "Spy",
		New: func(nodeIdx int, topo *Topology, slo levee.SLO) Governor {
			n := topo.Nodes[nodeIdx]
			return &spyGovernor{node: n.Name, rec: rec, replicas: n.MinReplicas,
				trackers: []*RoleTracker{{Name: "in:" + n.Name}}}
		},
	}
}

func (g *spyGovernor) InboundStart(ts time.Time) (int, error) {
	if g.replicas == 0 {
		return -1, levee.ErrCircuitOpen
	}
	rep := g.rr
	g.rr = (g.rr + 1) % g.replicas
	*g.rec = append(*g.rec, spyEvent{node: g.node, kind: "in-start", rep: rep, tsNS: ts.UnixNano()})
	return rep, nil
}

func (g *spyGovernor) InboundDone(rep int, ts time.Time, d time.Duration, ok bool) {
	*g.rec = append(*g.rec, spyEvent{node: g.node, kind: "in-done", rep: rep, tsNS: ts.UnixNano(), ok: ok, dur: d})
}

func (g *spyGovernor) OutboundStart(rep, edge int, ts time.Time) error {
	*g.rec = append(*g.rec, spyEvent{node: g.node, kind: "out-start", rep: rep, edge: edge, tsNS: ts.UnixNano()})
	return nil
}

func (g *spyGovernor) OutboundDone(rep, edge int, ts time.Time, d time.Duration, ok bool) {
	*g.rec = append(*g.rec, spyEvent{node: g.node, kind: "out-done", rep: rep, edge: edge, tsNS: ts.UnixNano(), ok: ok, dur: d})
}

func (g *spyGovernor) Resize(replicas int, tsNS int64) {
	g.replicas = replicas
	if g.rr >= replicas {
		g.rr = 0
	}
}

func (g *spyGovernor) Roles() []*RoleTracker { return g.trackers }

func countSpy(rec []spyEvent, node, kind string) int {
	n := 0
	for _, ev := range rec {
		if ev.node == node && ev.kind == kind {
			n++
		}
	}
	return n
}

// ampleNode returns a node spec that will not crash under test loads.
func ampleNode(name string, entryRPS float64, edges ...EdgeSpec) NodeSpec {
	return NodeSpec{Name: name, EntryRPS: entryRPS, PerReplicaRPS: 100,
		MinReplicas: 4, MaxReplicas: 8, P50MS: 10, P99MS: 40, Edges: edges}
}

func TestConservation(t *testing.T) {
	topo := NewTopology([]NodeSpec{ampleNode("solo", 100)})
	m := NewEngine(topo, NewNoGovCandidate(), nil, testSLO, DefaultSeed).Run(10)
	s := m.Mesh.Snapshot()
	if s.TotalBlocked != 0 {
		t.Errorf("no-gov blocked %d requests", s.TotalBlocked)
	}
	if s.TotalAllowed == 0 {
		t.Fatal("no arrivals simulated")
	}
	if s.TotalSuccesses+s.TotalFailures != s.TotalAllowed {
		t.Errorf("conservation: successes %d + failures %d != allowed %d",
			s.TotalSuccesses, s.TotalFailures, s.TotalAllowed)
	}
}

func TestChainOutcomePropagation(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		ampleNode("a", 50, EdgeSpec{Callee: "b", Prob: 1.0}),
		ampleNode("b", 0, EdgeSpec{Callee: "c", Prob: 1.0}),
		ampleNode("c", 0),
	})
	var rec []spyEvent
	phases := []Phase{{Name: "fail-c", StartS: 0, EndS: 60, ExtraErrorRate: map[string]float64{"c": 1.0}}}
	m := NewEngine(topo, newSpyCandidate(&rec), phases, testSLO, DefaultSeed).Run(5)

	s := m.Mesh.Snapshot()
	if s.TotalSuccesses != 0 {
		t.Errorf("all roots should fail with c erroring: %d successes", s.TotalSuccesses)
	}
	for _, ev := range rec {
		if ev.kind == "in-done" && ev.node == "a" && ev.ok {
			t.Error("a's inbound governor recorded a success despite downstream failure")
		}
		if ev.kind == "out-done" && ev.node == "b" && ev.ok {
			t.Error("b's outbound governor recorded a success from failing c")
		}
	}
	if countSpy(rec, "a", "in-done") != int(s.TotalAllowed) {
		t.Errorf("a in-done count %d != allowed %d", countSpy(rec, "a", "in-done"), s.TotalAllowed)
	}
}

func TestParallelFanoutFinishInvariant(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		ampleNode("a", 20, EdgeSpec{Callee: "b", Prob: 1.0}, EdgeSpec{Callee: "c", Prob: 1.0}),
		ampleNode("b", 0),
		ampleNode("c", 0),
	})
	var rec []spyEvent
	NewEngine(topo, newSpyCandidate(&rec), nil, testSLO, DefaultSeed).Run(5)

	// The parent's inbound Done must coincide with its last child's edge Done.
	outDoneTimes := map[int64]bool{}
	for _, ev := range rec {
		if ev.node == "a" && ev.kind == "out-done" {
			outDoneTimes[ev.tsNS] = true
		}
	}
	inDone := 0
	for _, ev := range rec {
		if ev.node == "a" && ev.kind == "in-done" {
			inDone++
			if !outDoneTimes[ev.tsNS] {
				t.Fatalf("a in-done at %d has no coinciding out-done (parent should finish with its last child)", ev.tsNS)
			}
		}
	}
	if inDone == 0 {
		t.Fatal("no completed roots")
	}
	if got := countSpy(rec, "a", "out-start"); got != 2*inDone {
		t.Errorf("expected 2 out-starts per root: got %d for %d roots", got, inDone)
	}
}

func TestTimeoutIsOnlyCrashSignal(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		ampleNode("a", 20, EdgeSpec{Callee: "b", Prob: 1.0}),
		// Slow and tiny: service rate ~4/s against ~20 RPS, buffer 10 -> crash.
		{Name: "b", PerReplicaRPS: 2, MinReplicas: 1, MaxReplicas: 1, P50MS: 500, P99MS: 900},
	})
	var rec []spyEvent
	eng := NewEngine(topo, newSpyCandidate(&rec), nil, testSLO, DefaultSeed)
	m := eng.Run(10)

	if m.Health[1].Crashes == 0 {
		t.Fatal("b should have crashed from queue saturation")
	}
	// Upstream failure signal must be the timeout: full remaining budget.
	timeoutFails := 0
	for _, ev := range rec {
		if ev.node == "a" && ev.kind == "out-done" && !ev.ok && ev.dur >= 1400*time.Millisecond {
			timeoutFails++
		}
	}
	if timeoutFails < 50 {
		t.Errorf("expected most parked calls to fail as timeouts, got %d", timeoutFails)
	}
	// A crashed node observes nothing: b's last inbound event precedes recovery.
	lastBStart := int64(0)
	for _, ev := range rec {
		if ev.node == "b" && ev.kind == "in-start" && ev.tsNS > lastBStart {
			lastBStart = ev.tsNS
		}
	}
	if lastBStart > 2e9 {
		t.Errorf("b admitted a request at %.2fs, after it should have crashed", float64(lastBStart)/1e9)
	}
}

func TestTimeoutDoesNotFreeCapacity(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		ampleNode("a", 2, EdgeSpec{Callee: "b", Prob: 1.0}),
		ampleNode("b", 0),
	})
	var rec []spyEvent
	// Service times x400 (>= 2s each) exceed the 1.5s budget for every call.
	phases := []Phase{{Name: "slow-a", StartS: 0, EndS: 60, LatencyMult: map[string]float64{"a": 400}}}
	eng := NewEngine(topo, newSpyCandidate(&rec), phases, testSLO, DefaultSeed)
	m := eng.Run(3)

	s := m.Mesh.Snapshot()
	if s.TotalSuccesses != 0 || s.TotalFailures != s.TotalAllowed {
		t.Errorf("all roots should time out: %d successes, %d/%d failures",
			s.TotalSuccesses, s.TotalFailures, s.TotalAllowed)
	}
	// Occupancy outlives the logical timeout: work drains well past sim end.
	if eng.nodes[0].lastConcNS < eng.endNS+2e9 {
		t.Errorf("a's occupancy ended at %.2fs; timed-out work should have held slots past %.2fs",
			float64(eng.nodes[0].lastConcNS)/1e9, float64(eng.endNS+2e9)/1e9)
	}
	// Deadline propagation: no children dispatched after the root timed out.
	if got := countSpy(rec, "b", "in-start"); got != 0 {
		t.Errorf("b received %d calls; timed-out parents must not dispatch children", got)
	}
}

func TestAsyncCycleTTL(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		ampleNode("x", 20, EdgeSpec{Callee: "y", Prob: 1.0, Async: true}),
		ampleNode("y", 0, EdgeSpec{Callee: "x", Prob: 1.0, Async: true}),
	})
	var rec []spyEvent
	m := NewEngine(topo, newSpyCandidate(&rec), nil, testSLO, DefaultSeed).Run(5)

	roots := int(m.Mesh.Snapshot().TotalAllowed)
	if roots == 0 {
		t.Fatal("no roots")
	}
	// Budget 8 admits exactly 8 hops per root through the p=1.0 cycle.
	total := countSpy(rec, "x", "in-start") + countSpy(rec, "y", "in-start")
	if total != 8*roots {
		t.Errorf("hop budget: got %d deliveries for %d roots, want %d", total, roots, 8*roots)
	}
}

func TestCrashOnSaturationAndRecovery(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		// Buffer = 5*10*1 = 50 against ~100 RPS inflow with slow service.
		{Name: "frail", EntryRPS: 100, PerReplicaRPS: 10, MinReplicas: 1, MaxReplicas: 4, P50MS: 400, P99MS: 800},
	})
	eng := NewEngine(topo, NewNoGovCandidate(), nil, testSLO, DefaultSeed)
	m := eng.Run(100)

	if m.Health[0].Crashes != 1 {
		t.Fatalf("expected exactly 1 crash within 100s (recovery takes 120s), got %d", m.Health[0].Crashes)
	}
	// Conservation holds through crashes: parked roots are allowed+failed.
	s := m.Mesh.Snapshot()
	if s.TotalSuccesses+s.TotalFailures != s.TotalAllowed {
		t.Errorf("conservation through crash: %d + %d != %d", s.TotalSuccesses, s.TotalFailures, s.TotalAllowed)
	}
	downS := float64(m.Health[0].DowntimeNS) / 1e9
	if downS < 98 || downS > 100 {
		t.Errorf("downtime %.1fs; crash should occur within ~2s and last through sim end", downS)
	}
	if m.MeanHealthyPct() > 5 {
		t.Errorf("healthy pct %.1f%% too high for a node crashed almost all run", m.MeanHealthyPct())
	}
}

func TestRecoveryRestartsAtMinReplicas(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		{Name: "frail", EntryRPS: 100, PerReplicaRPS: 10, MinReplicas: 1, MaxReplicas: 4, P50MS: 400, P99MS: 800},
	})
	eng := NewEngine(topo, NewNoGovCandidate(), nil, testSLO, DefaultSeed)
	eng.Run(125) // crash ~1s, recovery ~121s, provisioning lag keeps replicas at min
	if got := eng.nodes[0].cc.CurrentReplicas(); got != 1 {
		t.Errorf("replicas after recovery: got %d want MinReplicas (1)", got)
	}
	if eng.nodes[0].crashed {
		t.Error("node should have recovered")
	}
}

func TestDeterminism(t *testing.T) {
	topo := StorefrontTopology()
	run := func(seed uint64) (float64, int64, int64) {
		m := NewEngine(topo, NewLeveeCandidate(), DefaultScenario(), testSLO, seed).Run(120)
		s := m.Mesh.Snapshot()
		return m.MeshDelta(), s.TotalAllowed, s.TotalSuccesses
	}
	d1, a1, s1 := run(DefaultSeed)
	d2, a2, s2 := run(DefaultSeed)
	if d1 != d2 || a1 != a2 || s1 != s2 {
		t.Errorf("same seed diverged: (%v,%d,%d) vs (%v,%d,%d)", d1, a1, s1, d2, a2, s2)
	}
	_, a3, _ := run(DefaultSeed + 1)
	if a1 == a3 {
		t.Error("different seed produced identical arrival counts; rng may be unused")
	}
}

func TestGovernorReplicaScalingAndRR(t *testing.T) {
	// Steady 80 RPS on a MinReplicas=1 node forces a scale-up to 2 replicas
	// (target util 0.7 * 100); governor instances must follow.
	topo := NewTopology([]NodeSpec{
		{Name: "grow", EntryRPS: 80, PerReplicaRPS: 100, MinReplicas: 1, MaxReplicas: 4, P50MS: 10, P99MS: 40},
	})
	var rec []spyEvent
	eng := NewEngine(topo, newSpyCandidate(&rec), nil, testSLO, DefaultSeed)
	eng.Run(120)

	if got := eng.nodes[0].cc.CurrentReplicas(); got < 2 {
		t.Fatalf("node should have scaled up, has %d replicas", got)
	}
	if eng.nodes[0].govReplicas != eng.nodes[0].cc.CurrentReplicas() {
		t.Errorf("governor instances %d != replicas %d", eng.nodes[0].govReplicas, eng.nodes[0].cc.CurrentReplicas())
	}
	// After scale-up, admissions round-robin across instance indices.
	perRep := map[int]int{}
	for _, ev := range rec {
		if ev.kind == "in-start" && ev.tsNS > 100e9 {
			perRep[ev.rep]++
		}
	}
	if len(perRep) < 2 {
		t.Fatalf("expected admissions on >= 2 replicas late in the run, got %v", perRep)
	}
	for rep, cnt := range perRep {
		if cnt == 0 {
			t.Errorf("replica %d received no traffic", rep)
		}
	}
}

func TestLeveeGovernorScalesInstances(t *testing.T) {
	topo := StorefrontTopology()
	g := NewLeveeCandidate().New(topo.NodeIndex("db"), topo, testSLO).(*leveeGovernor)
	if len(g.instances) != 3 { // db MinReplicas
		t.Fatalf("initial instances: got %d want 3", len(g.instances))
	}
	g.Resize(5, 10e9)
	if len(g.instances) != 5 {
		t.Fatalf("after grow: got %d want 5", len(g.instances))
	}
	g.Resize(0, 20e9) // crash kills all instances
	if len(g.instances) != 0 {
		t.Fatalf("after crash: got %d want 0", len(g.instances))
	}
	if _, err := g.InboundStart(time.Unix(0, 21e9)); err == nil {
		t.Error("no instances should reject admission")
	}
	g.Resize(3, 25e9)
	if len(g.instances) != 3 {
		t.Fatalf("after recovery: got %d want 3", len(g.instances))
	}
	// Retired trackers stay retired; roles grow with respawned instances.
	if len(g.Roles()) != 8 { // (3 + 2 grown + 3 fresh) x 1 role each (db has no edges)
		t.Errorf("tracker count: got %d want 8", len(g.Roles()))
	}
	for _, rt := range g.Roles()[:5] {
		if !rt.retired {
			t.Error("pre-crash tracker should be retired")
		}
	}
}

func TestCrossCandidateComparability(t *testing.T) {
	topo := StorefrontTopology()
	steady, err := topo.SteadyStateRPS()
	if err != nil {
		t.Fatal(err)
	}
	arrivals := func(c Candidate) int64 {
		s := NewEngine(topo, c, nil, testSLO, DefaultSeed).Run(30).Mesh.Snapshot()
		return s.TotalAllowed + s.TotalBlocked
	}
	noGov := arrivals(NewNoGovCandidate())
	lev := arrivals(NewLeveeCandidate())
	static := arrivals(NewStaticCandidate("Static", steady))
	if noGov != lev || noGov != static {
		t.Errorf("entry arrivals must be candidate-independent: no-gov %d, levee %d, static %d", noGov, lev, static)
	}
}
