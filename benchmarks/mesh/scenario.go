package mesh

// Phase describes one scenario window with optional perturbations.
type Phase struct {
	Name           string
	StartS, EndS   int
	EntryRateMult  map[string]float64 // entry node -> arrival rate multiplier
	LatencyMult    map[string]float64 // node -> service time multiplier
	ExtraErrorRate map[string]float64 // node -> additive local error probability
}

// Scenario tuning knobs; see mesh_benchmark.md for the calibration rationale.
const (
	SurgeMult        = 4.0 // phase B multiplier on the high-throughput entry
	degradeLatMult   = 4.0 // phase C service-time inflation at db
	degradeErrRate   = 0.15
	crashLatMult     = 8.0 // phase D service-time inflation at payments
	FullScenarioEndS = 1200
	ShortScenarioEndS = 330 // phases A + B only
)

// DefaultScenario is the 20-minute 5-phase mesh stress timeline.
func DefaultScenario() []Phase {
	return []Phase{
		{Name: "A-steady", StartS: 0, EndS: 180},
		{Name: "B-surge", StartS: 180, EndS: 330,
			EntryRateMult: map[string]float64{"edge-api": SurgeMult}},
		{Name: "B-settle", StartS: 330, EndS: 420},
		{Name: "C-degrade", StartS: 420, EndS: 570,
			LatencyMult:    map[string]float64{"db": degradeLatMult},
			ExtraErrorRate: map[string]float64{"db": degradeErrRate}},
		{Name: "C-settle", StartS: 570, EndS: 660},
		{Name: "D-crash", StartS: 660, EndS: 780,
			LatencyMult: map[string]float64{"payments": crashLatMult}},
		{Name: "E-recovery", StartS: 780, EndS: 1200},
	}
}

// Phase lookups are precompiled by the engine into per-node arrays; see
// Engine.rateMultAt, latMultAt, extraErrAt.
