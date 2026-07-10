package benchmarks_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks/mesh"
)

// meshSLO matches the distributed suite so Deltas are comparable.
var meshSLO = levee.SLO{SuccessRate: 0.90, Timeout: 1500 * time.Millisecond}

type meshResult struct {
	name string
	m    *mesh.MeshMetrics
	wall time.Duration
}

// meshCandidates builds the five competitors. The static candidates deploy
// breakers only, limiters only, or both, all sized from the provisioning
// profile alone (see mesh/static.go).
func meshCandidates(t *testing.T, topo *mesh.Topology) []mesh.Candidate {
	t.Helper()
	if err := topo.Validate(); err != nil {
		t.Fatalf("topology: %v", err)
	}
	return []mesh.Candidate{
		mesh.NewNoGovCandidate(),
		mesh.NewStaticCandidate("Static-Breaker", mesh.StaticBreaker),
		mesh.NewStaticCandidate("Static-Limiter", mesh.StaticLimiter),
		mesh.NewStaticCandidate("Static-Full", mesh.StaticBreaker|mesh.StaticLimiter),
		mesh.NewLeveeCandidate(),
	}
}

func runMeshBenchmark(t *testing.T, endS int) []meshResult {
	t.Helper()
	topo := mesh.StorefrontTopology()
	cands := meshCandidates(t, topo)
	ch := make(chan meshResult, len(cands))
	for _, cand := range cands {
		go func(c mesh.Candidate) {
			start := time.Now()
			m := mesh.NewEngine(topo, c, mesh.DefaultScenario(), meshSLO, mesh.DefaultSeed).Run(endS)
			ch <- meshResult{name: c.Name, m: m, wall: time.Since(start)}
		}(cand)
	}
	byName := make(map[string]meshResult, len(cands))
	for range cands {
		r := <-ch
		byName[r.name] = r
	}
	results := make([]meshResult, 0, len(cands))
	for _, c := range cands {
		results = append(results, byName[c.Name])
	}
	return results
}

func printMeshResults(t *testing.T, results []meshResult, endS int) {
	t.Helper()
	sep := strings.Repeat("-", 100)

	t.Logf("Mesh benchmark: %d nodes, %d simulated seconds", len(results[0].m.Health), endS)
	t.Log(sep)
	t.Logf("%-15s | %9s | %9s | %9s | %9s | %10s | %8s",
		"Candidate", "Allowed", "Blocked", "Success", "Failures", "MeshDelta", "Wall")
	t.Log(sep)
	for _, r := range results {
		s := r.m.Mesh.Snapshot()
		t.Logf("%-15s | %9d | %9d | %9d | %9d | %10.1f | %8s",
			r.name, s.TotalAllowed, s.TotalBlocked, s.TotalSuccesses, s.TotalFailures, r.m.MeshDelta(),
			r.wall.Round(time.Millisecond))
	}
	t.Log(sep)

	t.Log("Per-entry Delta:")
	for _, r := range results {
		line := r.name + ":"
		for _, name := range r.m.EntryNames {
			line += " " + name + "=" + formatFloat(r.m.EntryDelta(name))
		}
		t.Log("  " + line)
	}

	t.Log("Node crashes (candidate: node xN, downtime%):")
	for _, r := range results {
		line := ""
		for _, h := range r.m.Health {
			if h.Crashes > 0 {
				downtime := 100 * float64(h.DowntimeNS) / float64(r.m.SimNS)
				line += fmt.Sprintf(" %s x%d (%s%%)", h.Name, h.Crashes, formatFloat(downtime))
			}
		}
		if line == "" {
			line = " none"
		}
		t.Logf("  %-15s:%s", r.name, line)
	}

	for _, r := range results {
		if r.name != "Levee" {
			continue
		}
		// Aggregate across replica instances sharing a role name.
		type agg struct {
			dur         [3]int64
			transitions int
		}
		byRole := map[string]*agg{}
		var order []string
		for _, rt := range r.m.Roles {
			a := byRole[rt.Name]
			if a == nil {
				a = &agg{}
				byRole[rt.Name] = a
				order = append(order, rt.Name)
			}
			for b := range 3 {
				a.dur[b] += rt.DurNS[b]
			}
			a.transitions += rt.Transitions
		}
		t.Log("Levee roles with non-CLOSED dwell (>0.5%, aggregated over replicas):")
		for _, name := range order {
			a := byRole[name]
			total := a.dur[0] + a.dur[1] + a.dur[2]
			if total == 0 {
				continue
			}
			th := 100 * float64(a.dur[1]) / float64(total)
			op := 100 * float64(a.dur[2]) / float64(total)
			if th+op > 0.5 {
				t.Logf("  %-28s throttled=%6.2f%% open=%6.2f%% transitions=%d", name, th, op, a.transitions)
			}
		}
	}

	// CSV block for extraction.
	t.Log("CSV:")
	t.Log("Candidate,Allowed,Blocked,Successes,Failures,MeshDelta")
	for _, r := range results {
		s := r.m.Mesh.Snapshot()
		t.Logf("%s,%d,%d,%d,%d,%.1f",
			r.name, s.TotalAllowed, s.TotalBlocked, s.TotalSuccesses, s.TotalFailures, r.m.MeshDelta())
	}
}

func compareMesh(t *testing.T, results []meshResult, assert bool) {
	t.Helper()
	var lev, best *meshResult
	for i := range results {
		r := &results[i]
		switch {
		case r.name == "Levee":
			lev = r
		case strings.HasPrefix(r.name, "Static-"):
			if best == nil || r.m.MeshDelta() > best.m.MeshDelta() {
				best = r
			}
		}
	}
	ld, bd := lev.m.MeshDelta(), best.m.MeshDelta()
	verdict := "LOSS"
	if ld > bd {
		verdict = "WIN"
	}
	t.Logf("Levee vs best static (%s) MeshDelta: Levee=%.1f %s=%.1f Lead=%.1f %s",
		best.name, ld, best.name, bd, ld-bd, verdict)
	if assert && ld <= bd {
		t.Errorf("Levee MeshDelta %.1f did not beat %s %.1f at the reference config", ld, best.name, bd)
	}
}

func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}

// TestMeshBenchmark runs the full 30-minute mesh scenario.
func TestMeshBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full mesh benchmark in short mode")
	}
	results := runMeshBenchmark(t, mesh.FullScenarioEndS)
	printMeshResults(t, results, mesh.FullScenarioEndS)
	compareMesh(t, results, true)
}

// TestMeshBenchmarkShort runs phases A+B only, for quick iteration.
func TestMeshBenchmarkShort(t *testing.T) {
	results := runMeshBenchmark(t, mesh.ShortScenarioEndS)
	printMeshResults(t, results, mesh.ShortScenarioEndS)
	compareMesh(t, results, false)
}
