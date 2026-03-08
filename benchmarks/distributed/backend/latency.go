package backend

import (
	"math"
	"math/rand/v2"
	"sync"

	"github.com/codemartial/loadgen"
)

const (
	// healthyErrorRate is the baseline error rate during healthy operation (0.5%)
	healthyErrorRate = 0.005
)

// bauDegradation returns the BAU degradation multiplier for a given hour.
// This affects both latency and error rates.
// Phase 1: 1x -> 1.5x between 10 AM spike (hour 10) and 6 PM spike (hour 18)
// Phase 2: 1.5x -> 3x after 6 PM spike (hours 18-23)
// Note: Duplicated from benchmarks package to avoid circular dependency.
func bauDegradation(hour int) float64 {
	if hour < 10 {
		return 1.0
	}
	if hour < 18 {
		// Linear creep from 1.0 at hour 10 to 1.5 at hour 18
		return 1.0 + 0.5*float64(hour-10)/8.0
	}
	// Linear creep from 1.5 at hour 18 to 3.0 at hour 23
	return 1.5 + 1.5*float64(hour-18)/5.0
}

// LatencyProfile holds the parameters for generating latencies.
type LatencyProfile struct {
	P50MS     float64
	P99MS     float64
	TimeoutMS float64

	// Derived rational function parameters: y = a + b*x/(1-c*x)
	a, b, c float64
}

// NewLatencyProfile creates a latency profile from P50/P99/Timeout.
func NewLatencyProfile(p50, p99, timeout float64) LatencyProfile {
	lp := LatencyProfile{
		P50MS:     p50,
		P99MS:     p99,
		TimeoutMS: timeout,
	}

	// From loadgen.go:
	// a = p50/2 (constraint: x=0 -> y = p50/2)
	// c = (1.49*p50 - p99) / (0.99*(p50 - p99))
	// b = p50 * (1 - 0.5*c)
	lp.a = p50 / 2
	lp.c = (1.49*p50 - p99) / (0.99 * (p50 - p99))
	lp.b = p50 * (1 - 0.5*lp.c)

	return lp
}

// MeanLatencyMS returns the analytical mean latency in milliseconds.
// Integrates the piecewise distribution: rational function over [0, 0.99]
// and linear tail over [0.99, 1.0].
func (lp LatencyProfile) MeanLatencyMS() float64 {
	// Integral of (a + b*x/(1-c*x)) from 0 to 0.99
	var rationalIntegral float64
	if math.Abs(lp.c) < 1e-10 {
		// c ≈ 0: y = a + b*x, integral = a*0.99 + b*0.99²/2
		rationalIntegral = lp.a*0.99 + lp.b*0.99*0.99/2.0
	} else {
		// ∫(a + b*x/(1-c*x))dx = a*x + b*[-x/c - ln(1-c*x)/c²]
		rationalIntegral = lp.a*0.99 + lp.b*(-0.99/lp.c-math.Log(1-0.99*lp.c)/(lp.c*lp.c))
	}

	// Integral of linear tail from 0.99 to 1.0
	// y = P99 + slope*(x - 0.99), slope = (Timeout - P99) / 0.01
	slope := (lp.TimeoutMS - lp.P99MS) / 0.01
	tailIntegral := lp.P99MS*0.01 + slope*0.00005

	return rationalIntegral + tailIntegral
}

// LatencyGenerator generates realistic latencies using the rational function distribution.
type LatencyGenerator struct {
	mu                   sync.Mutex
	profiles             []LatencyProfile // One per spec
	healthyLatencyLookup map[int]int      // incident spec idx -> healthy spec idx
	baselineErrorRates   []float64        // Baseline error rate per spec (follows BAU degradation trend)
	rng                  *rand.Rand
}

// NewLatencyGenerator creates a latency generator from load specs.
// It pre-computes which specs are incidents and maps them to subsequent healthy latency profiles.
// It also pre-computes baseline error rates based on the BAU degradation trend.
func NewLatencyGenerator(specs []loadgen.LoadSpec, seed uint64) *LatencyGenerator {
	lg := &LatencyGenerator{
		profiles:             make([]LatencyProfile, len(specs)),
		healthyLatencyLookup: make(map[int]int),
		baselineErrorRates:   make([]float64, len(specs)),
		rng:                  rand.New(rand.NewPCG(seed, seed>>32)),
	}

	// Create latency profiles for each spec
	for i, spec := range specs {
		lg.profiles[i] = NewLatencyProfile(spec.P50LatencyMS, spec.P99LatencyMS, spec.TimeoutMS)
	}

	// Identify incidents (ErrorRate > 10% SLO threshold) and map to subsequent healthy profile
	for i, spec := range specs {
		if isIncidentSpec(spec) {
			// Find next healthy spec
			for j := i + 1; j < len(specs); j++ {
				if !isIncidentSpec(specs[j]) {
					lg.healthyLatencyLookup[i] = j
					break
				}
			}
			// If no subsequent healthy spec found, we'll use the current one
		}
	}

	// Pre-compute baseline error rates based on spec start times
	// The baseline follows the BAU degradation trend, ignoring spike-induced error rates
	var cumulativeSeconds int64
	for i, spec := range specs {
		// Calculate the hour at which this spec starts
		// Note: The workload starts at "8 PM Sunday" which is hour -4 relative to midnight
		// But BauDegradation uses hour 0-23 for Cyber Monday, so we offset by 4 hours
		hour := int((cumulativeSeconds / 3600) - 4) // -4 to 23 range
		if hour < 0 {
			hour = 0 // Before midnight, use hour 0 degradation (1.0x)
		}
		if hour > 23 {
			hour = 23 // Cap at hour 23
		}

		// Baseline error rate follows the degradation trend
		lg.baselineErrorRates[i] = healthyErrorRate * bauDegradation(hour)

		cumulativeSeconds += int64(spec.DurationS)
	}

	return lg
}

// isIncidentSpec returns true if the spec represents an incident period.
func isIncidentSpec(spec loadgen.LoadSpec) bool {
	return spec.ErrorRate > 0.10 // SLO threshold
}

// Sample generates a latency in milliseconds for the given spec index.
// For incident specs, uses the latency profile of the subsequent healthy spec.
func (lg *LatencyGenerator) Sample(specIndex int) float64 {
	profileIdx := specIndex

	// If this is an incident spec, use the healthy latency profile
	if healthyIdx, isIncident := lg.healthyLatencyLookup[specIndex]; isIncident {
		profileIdx = healthyIdx
	}

	// Bounds check
	if profileIdx < 0 || profileIdx >= len(lg.profiles) {
		profileIdx = len(lg.profiles) - 1
	}

	return lg.sampleProfile(lg.profiles[profileIdx])
}

// sampleProfile generates a latency from the given profile.
func (lg *LatencyGenerator) sampleProfile(profile LatencyProfile) float64 {
	lg.mu.Lock()
	x := lg.rng.Float64()
	lg.mu.Unlock()

	var latencyMS float64
	if x <= 0.99 {
		// Rational function for main distribution
		latencyMS = profile.a + profile.b*x/(1-profile.c*x)
	} else {
		// Linear tail from P99 (at x=0.99) to timeout (at x=1.0)
		slope := (profile.TimeoutMS - profile.P99MS) / 0.01
		latencyMS = profile.P99MS + slope*(x-0.99)
	}

	// Clamp to timeout
	if latencyMS > profile.TimeoutMS {
		latencyMS = profile.TimeoutMS
	}
	if latencyMS < 0 {
		latencyMS = 0
	}

	return latencyMS
}

// GetProfile returns the effective latency profile for a spec index.
func (lg *LatencyGenerator) GetProfile(specIndex int) LatencyProfile {
	profileIdx := specIndex
	if healthyIdx, isIncident := lg.healthyLatencyLookup[specIndex]; isIncident {
		profileIdx = healthyIdx
	}
	if profileIdx < 0 || profileIdx >= len(lg.profiles) {
		profileIdx = len(lg.profiles) - 1
	}
	return lg.profiles[profileIdx]
}

// IsIncident returns true if the spec index represents an incident period.
func (lg *LatencyGenerator) IsIncident(specIndex int) bool {
	_, isIncident := lg.healthyLatencyLookup[specIndex]
	return isIncident
}

// ShouldError returns true if this request should fail due to baseline errors.
// The baseline error rate follows the BAU degradation trend, independent of
// whether the spec is an incident period. This simulates the inherent error
// rate of the system even when operating normally.
func (lg *LatencyGenerator) ShouldError(specIndex int) bool {
	if specIndex < 0 || specIndex >= len(lg.baselineErrorRates) {
		return false
	}
	lg.mu.Lock()
	r := lg.rng.Float64()
	lg.mu.Unlock()
	return r < lg.baselineErrorRates[specIndex]
}

// GetBaselineErrorRate returns the baseline error rate for a spec index.
func (lg *LatencyGenerator) GetBaselineErrorRate(specIndex int) float64 {
	if specIndex < 0 || specIndex >= len(lg.baselineErrorRates) {
		return healthyErrorRate
	}
	return lg.baselineErrorRates[specIndex]
}

// RollFloat64 returns a random float64 in [0, 1). Thread-safe.
func (lg *LatencyGenerator) RollFloat64() float64 {
	lg.mu.Lock()
	r := lg.rng.Float64()
	lg.mu.Unlock()
	return r
}
