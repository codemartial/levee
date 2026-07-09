package mesh

// Phase describes one scenario window with optional perturbations.
type Phase struct {
	Name           string
	StartS, EndS   int
	EntryRateMult  map[string]float64 // entry node -> arrival rate multiplier
	LatencyMult    map[string]float64 // node -> service time multiplier
	ExtraErrorRate map[string]float64 // node -> additive local error probability
	EdgeCallMult   map[string]float64 // "caller->callee" -> calls-per-request multiplier
	RampS          int                // linear ramp-in seconds for EntryRateMult (edge-network smoothing)
}

// Scenario tuning knobs; see mesh_benchmark.md for the calibration rationale.
const (
	SurgeMult         = 4.0 // phase B multiplier on the high-throughput entry
	degradeLatMult    = 4.0 // phase C service-time inflation at db
	degradeErrRate    = 0.15
	crashLatMult      = 8.0  // phase D service-time inflation at payments
	OvercapMult       = 8.0  // phase F entry surge beyond edge-api MaxReplicas capacity
	bugCallMult       = 10.0 // phase G catalog->inventory calls per request (code-bug storm)
	surgeRampS        = 5    // arrival ramp for surge phases (edge-network smoothing)
	FullScenarioEndS  = 1800
	ShortScenarioEndS = 330 // phases A + B only
)

// DefaultScenario is the 30-minute mesh stress timeline.
func DefaultScenario() []Phase {
	return []Phase{
		{Name: "A-steady", StartS: 0, EndS: 180},
		{Name: "B-surge", StartS: 180, EndS: 330,
			EntryRateMult: map[string]float64{"edge-api": SurgeMult}, RampS: surgeRampS},
		{Name: "B-settle", StartS: 330, EndS: 420},
		{Name: "C-degrade", StartS: 420, EndS: 570,
			LatencyMult:    map[string]float64{"db": degradeLatMult},
			ExtraErrorRate: map[string]float64{"db": degradeErrRate}},
		{Name: "C-settle", StartS: 570, EndS: 660},
		{Name: "D-crash", StartS: 660, EndS: 780,
			LatencyMult: map[string]float64{"payments": crashLatMult}},
		{Name: "E-recovery", StartS: 780, EndS: 1200},
		{Name: "F-overcap", StartS: 1200, EndS: 1350,
			EntryRateMult: map[string]float64{"edge-api": OvercapMult}, RampS: surgeRampS},
		{Name: "F-settle", StartS: 1350, EndS: 1440},
		{Name: "G-bugstorm", StartS: 1440, EndS: 1590,
			EdgeCallMult: map[string]float64{"catalog->inventory": bugCallMult}},
		{Name: "H-recovery", StartS: 1590, EndS: 1800},
	}
}

// Phase lookups are precompiled by the engine into per-node arrays; see
// Engine.rateMultAt, latMultAt, extraErrAt, edgeMultAt.
