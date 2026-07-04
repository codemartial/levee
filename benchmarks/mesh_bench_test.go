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

// meshCandidates builds the four competitors. Static-Nominal is sized from
// steady-state design RPS; Static-Peak folds the surge into its sizing.
func meshCandidates(t *testing.T, topo *mesh.Topology) []mesh.Candidate {
	t.Helper()
	if err := topo.Validate(); err != nil {
		t.Fatalf("topology: %v", err)
	}
	steady, err := topo.SteadyStateRPS()
	if err != nil {
		t.Fatal(err)
	}
	steadyPeak, err := mesh.SurgeSteadyRPS(topo, "edge-api", mesh.SurgeMult)
	if err != nil {
		t.Fatal(err)
	}
	return []mesh.Candidate{
		mesh.NewNoGovCandidate(),
		mesh.NewLeveeCandidate(),
		mesh.NewStaticCandidate("Static-Nominal", steady),
		mesh.NewStaticCandidate("Static-Peak", steadyPeak),
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
	sep := strings.Repeat("-", 152)

	t.Logf("Mesh benchmark: %d nodes, %d simulated seconds", len(results[0].m.Health), endS)
	t.Log(sep)
	t.Logf("%-15s | %9s | %9s | %9s | %9s | %10s | %7s | %7s | %7s | %8s | %8s | %9s | %8s",
		"Candidate", "Allowed", "Blocked", "Success", "Failures", "MeshDelta",
		"Closed%", "Throt%", "Open%", "Healthy%", "AvgCrash", "ConcRatio", "Wall")
	t.Log(sep)
	for _, r := range results {
		s := r.m.Mesh.Snapshot()
		cl, th, op := r.m.StatePcts()
		t.Logf("%-15s | %9d | %9d | %9d | %9d | %10.1f | %7.2f | %7.2f | %7.2f | %8.2f | %8.2f | %9.1f | %8s",
			r.name, s.TotalAllowed, s.TotalBlocked, s.TotalSuccesses, s.TotalFailures, r.m.MeshDelta(),
			cl, th, op, r.m.MeanHealthyPct(), r.m.AvgCrashes(), r.m.MaxConcRatio(), r.wall.Round(time.Millisecond))
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

	t.Log("Node crashes (candidate: node xN, healthy%):")
	for _, r := range results {
		line := ""
		for _, h := range r.m.Health {
			if h.Crashes > 0 {
				healthy := 100 * (1 - float64(h.DowntimeNS)/float64(r.m.SimNS))
				line += fmt.Sprintf(" %s x%d (%s%%)", h.Name, h.Crashes, formatFloat(healthy))
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
	t.Log("Candidate,Allowed,Blocked,Successes,Failures,MeshDelta,ClosedPct,ThrottledPct,OpenPct,HealthyPct,AvgCrashes,MaxConcRatio")
	for _, r := range results {
		s := r.m.Mesh.Snapshot()
		cl, th, op := r.m.StatePcts()
		t.Logf("%s,%d,%d,%d,%d,%.1f,%.2f,%.2f,%.2f,%.2f,%.2f,%.1f",
			r.name, s.TotalAllowed, s.TotalBlocked, s.TotalSuccesses, s.TotalFailures, r.m.MeshDelta(),
			cl, th, op, r.m.MeanHealthyPct(), r.m.AvgCrashes(), r.m.MaxConcRatio())
	}
}

func compareMesh(t *testing.T, results []meshResult, assert bool) {
	t.Helper()
	var lev, peak *meshResult
	for i := range results {
		switch results[i].name {
		case "Levee":
			lev = &results[i]
		case "Static-Peak":
			peak = &results[i]
		}
	}
	ld, pd := lev.m.MeshDelta(), peak.m.MeshDelta()
	verdict := "LOSS"
	if ld > pd {
		verdict = "WIN"
	}
	t.Logf("Levee vs Static-Peak MeshDelta: Levee=%.1f Static-Peak=%.1f Lead=%.1f %s", ld, pd, ld-pd, verdict)
	t.Logf("Levee vs Static-Peak Healthy%%:  Levee=%.2f Static-Peak=%.2f", lev.m.MeanHealthyPct(), peak.m.MeanHealthyPct())
	t.Logf("Levee vs Static-Peak ConcRatio: Levee=%.1f Static-Peak=%.1f", lev.m.MaxConcRatio(), peak.m.MaxConcRatio())
	if assert && ld <= pd {
		t.Errorf("Levee MeshDelta %.1f did not beat Static-Peak %.1f at the reference config", ld, pd)
	}
}

func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}

// TestMeshBenchmark runs the full 20-minute 5-phase mesh scenario.
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
