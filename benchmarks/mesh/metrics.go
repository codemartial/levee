package mesh

import (
	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/levee/benchmarks/distributed/app"
)

// NodeHealthStat is per-node crash accounting over the sim window.
type NodeHealthStat struct {
	Name       string
	Crashes    int
	DowntimeNS int64
}

// MeshMetrics aggregates one candidate run. Entry scorers reuse the
// distributed suite's epoch scoring so Deltas are comparable across suites.
type MeshMetrics struct {
	EntryNames []string
	Entry      map[string]*app.CBMetrics
	Mesh       *app.CBMetrics
	Roles      []*RoleTracker
	Health     []NodeHealthStat
	SimNS      int64

	engine *Engine
}

func newMeshMetrics(e *Engine) *MeshMetrics {
	m := &MeshMetrics{
		Entry:  make(map[string]*app.CBMetrics),
		Mesh:   &app.CBMetrics{},
		Health: make([]NodeHealthStat, len(e.nodes)),
		engine: e,
	}
	for _, ni := range e.entries {
		name := e.topo.Nodes[ni].Name
		m.EntryNames = append(m.EntryNames, name)
		m.Entry[name] = &app.CBMetrics{}
	}
	for i, n := range e.topo.Nodes {
		m.Health[i].Name = n.Name
	}
	return m
}

func (m *MeshMetrics) entryScorer(slot int) *app.CBMetrics {
	return m.Entry[m.EntryNames[slot]]
}

func (m *MeshMetrics) recordAllowed(slot int, conc int64) {
	m.entryScorer(slot).RecordAllowed(conc, levee.CLOSED)
	m.Mesh.RecordAllowed(conc, levee.CLOSED)
}

func (m *MeshMetrics) recordBlocked(slot int) {
	m.entryScorer(slot).RecordBlocked()
	m.Mesh.RecordBlocked()
}

func (m *MeshMetrics) recordResult(slot int, tNS int64, ok bool) {
	m.entryScorer(slot).RecordResult(tNS, ok)
	m.entryScorer(slot).RecordCompletion(ok, 0)
	m.Mesh.RecordResult(tNS, ok)
	m.Mesh.RecordCompletion(ok, 0)
}

// addDowntime accumulates a crash window clamped to the sim end.
func (m *MeshMetrics) addDowntime(node int, sinceNS, untilNS, endNS int64) {
	if sinceNS > endNS {
		sinceNS = endNS
	}
	if untilNS > endNS {
		untilNS = endNS
	}
	if untilNS > sinceNS {
		m.Health[node].DowntimeNS += untilNS - sinceNS
	}
}

// finalize flushes scorers, trackers, and per-node counters.
func (m *MeshMetrics) finalize(endNS int64) {
	m.SimNS = endNS
	m.Mesh.Finalize()
	for _, name := range m.EntryNames {
		m.Entry[name].Finalize()
	}
	for i, n := range m.engine.nodes {
		m.Health[i].Crashes = n.crashCount
		for _, rt := range n.gov.Roles() {
			rt.Flush(endNS)
			m.Roles = append(m.Roles, rt)
		}
	}
	m.engine = nil
}

// Delta is the suite-standard score: (S - F) * Allowed / (Allowed + Blocked).
func Delta(s api.CBMetrics) float64 {
	total := s.TotalAllowed + s.TotalBlocked
	if total == 0 {
		return 0
	}
	return (s.SuccessScore - s.FailureScore) * float64(s.TotalAllowed) / float64(total)
}

// MeshDelta is the Delta over the combined stream across all entries.
func (m *MeshMetrics) MeshDelta() float64 {
	return Delta(m.Mesh.Snapshot())
}

// EntryDelta is the Delta for one entry point.
func (m *MeshMetrics) EntryDelta(name string) float64 {
	return Delta(m.Entry[name].Snapshot())
}

// MeanHealthyPct is the mean over nodes of percent lifetime not crashed.
func (m *MeshMetrics) MeanHealthyPct() float64 {
	if len(m.Health) == 0 || m.SimNS == 0 {
		return 100
	}
	sum := 0.0
	for _, h := range m.Health {
		sum += 100 * (1 - float64(h.DowntimeNS)/float64(m.SimNS))
	}
	return sum / float64(len(m.Health))
}
