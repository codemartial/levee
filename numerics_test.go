package levee

import (
	"math"
	"testing"
	"time"
)

// These tests pin the library's numerical core to independently-known values.
// They exist for two reasons: the helpers are otherwise only exercised
// indirectly through the breaker's behaviour, and the float math is exactly the
// kind that regressed across architectures before (see TestClosedUncapped). A
// reference constant that holds on both amd64 and arm64 catches that class of bug.

func approxEqual(a, b, tol float64) bool {
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	return math.Abs(a-b) <= tol
}

func TestInvNormCDFReferenceValues(t *testing.T) {
	const tol = 1e-7
	cases := []struct {
		p    float64
		want float64
	}{
		{0.5, 0.0},                  // median
		{0.8413447460685429, 1.0},   // Phi(1)
		{0.9772498680518208, 2.0},   // Phi(2)
		{0.95, 1.6448536269514722},  // common 95% one-sided z
		{0.975, 1.959963984540054},  // 97.5% one-sided z
		{0.99, 2.3263478740408408},  // 99%
		{0.001, -3.090232306167813}, // deep lower tail
		{0.90, 1.2815515594465004},  // 90%
	}
	for _, c := range cases {
		if got := invNormCDF(c.p); !approxEqual(got, c.want, tol) {
			t.Errorf("invNormCDF(%v) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestInvNormCDFSymmetry(t *testing.T) {
	const tol = 1e-9
	for _, p := range []float64{0.01, 0.1, 0.3, 0.5, 0.7, 0.9, 0.99} {
		lo, hi := invNormCDF(p), invNormCDF(1-p)
		if !approxEqual(lo, -hi, tol) {
			t.Errorf("invNormCDF not symmetric: invNormCDF(%v)=%v, -invNormCDF(%v)=%v", p, lo, 1-p, -hi)
		}
	}
}

func TestInvNormCDFExtremes(t *testing.T) {
	if got := invNormCDF(0); !math.IsInf(got, -1) {
		t.Errorf("invNormCDF(0) = %v, want -Inf", got)
	}
	if got := invNormCDF(-0.5); !math.IsInf(got, -1) {
		t.Errorf("invNormCDF(-0.5) = %v, want -Inf", got)
	}
	if got := invNormCDF(1); !math.IsInf(got, 1) {
		t.Errorf("invNormCDF(1) = %v, want +Inf", got)
	}
	if got := invNormCDF(1.5); !math.IsInf(got, 1) {
		t.Errorf("invNormCDF(1.5) = %v, want +Inf", got)
	}
}

func TestDeriveThresholds(t *testing.T) {
	// tripZ goes through math.Erfinv, whose accuracy is ~1e-8, so compare it
	// loosely; recoverThreshold and consecTrip are exact.
	const tol = 1e-6
	cases := []struct {
		successRate    float64
		wantTripZ      float64
		wantRecover    float64
		wantConsecTrip int
	}{
		{0.90, 1.2815515594465004, 0.10, 8},
		{0.95, 1.6448536269514722, 0.10, 7}, // sloErr 0.05 < 0.095 -> recover floored to 0.10
		{0.99, 2.3263478740408408, 0.10, 5}, // 1.0-0.99 is 0.0100000000000000009, so ceil(log) rounds up
		{0.80, 0.8416212335729143, 0.20, 12},
	}
	for _, c := range cases {
		tripZ, recoverTh, consecTrip := deriveThresholds(SLO{SuccessRate: c.successRate, Timeout: time.Second})
		if !approxEqual(tripZ, c.wantTripZ, tol) {
			t.Errorf("SLO %.2f: tripZ = %v, want %v", c.successRate, tripZ, c.wantTripZ)
		}
		if !approxEqual(recoverTh, c.wantRecover, tol) {
			t.Errorf("SLO %.2f: recoverThreshold = %v, want %v", c.successRate, recoverTh, c.wantRecover)
		}
		if consecTrip != c.wantConsecTrip {
			t.Errorf("SLO %.2f: consecTrip = %d, want %d", c.successRate, consecTrip, c.wantConsecTrip)
		}
	}
}

func TestEWMAAlpha(t *testing.T) {
	const tol = 1e-12
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
	l.goodput = 0 // no request-rate cap

	// At exactly one half-life the decay factor is 0.5; at two half-lives, 0.75.
	if got := l.ewmaAlpha(3.0, 3*time.Second); !approxEqual(got, 0.5, tol) {
		t.Errorf("ewmaAlpha(1 half-life) = %v, want 0.5", got)
	}
	if got := l.ewmaAlpha(6.0, 3*time.Second); !approxEqual(got, 0.75, tol) {
		t.Errorf("ewmaAlpha(2 half-lives) = %v, want 0.75", got)
	}
	// Non-positive dt yields the minimum floor.
	if got := l.ewmaAlpha(0, 3*time.Second); got != 0.01 {
		t.Errorf("ewmaAlpha(0) = %v, want 0.01", got)
	}
	if got := l.ewmaAlpha(-5, 3*time.Second); got != 0.01 {
		t.Errorf("ewmaAlpha(negative) = %v, want 0.01", got)
	}

	// With high goodput the per-request cap dominates and shrinks alpha.
	l.goodput = 1000
	maxAlpha := 1.0 - math.Exp(-(1.0/1000.0)*math.Ln2/3.0)
	if got := l.ewmaAlpha(3.0, 3*time.Second); !approxEqual(got, maxAlpha, tol) {
		t.Errorf("ewmaAlpha capped = %v, want %v", got, maxAlpha)
	}
}

func TestRequestRate(t *testing.T) {
	const tol = 1e-9
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
	l.goodput = 100

	l.errEWMA = 0
	if got := l.requestRate(); !approxEqual(got, 100, tol) {
		t.Errorf("requestRate(err=0) = %v, want 100", got)
	}
	l.errEWMA = 0.5
	if got := l.requestRate(); !approxEqual(got, 200, tol) {
		t.Errorf("requestRate(err=0.5) = %v, want 200", got)
	}
	// Denominator is floored at 0.05 near a full outage.
	l.errEWMA = 0.99
	if got := l.requestRate(); !approxEqual(got, 2000, tol) {
		t.Errorf("requestRate(err=0.99) = %v, want 2000 (floored denom)", got)
	}
}

func TestEffectiveSamples(t *testing.T) {
	const tol = 1e-6
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})

	// No goodput -> request rate 0 -> sample mass floored at 1.
	l.goodput = 0
	if got := l.effectiveSamples(); got != 1.0 {
		t.Errorf("effectiveSamples(goodput=0) = %v, want 1", got)
	}

	// effectiveSamples = 2 * requestRate * halfLife / ln2, with requestRate=100.
	l.goodput, l.errEWMA = 100, 0
	want := 2.0 * 100.0 * 3.0 / math.Ln2
	if got := l.effectiveSamples(); !approxEqual(got, want, tol) {
		t.Errorf("effectiveSamples = %v, want %v", got, want)
	}
}

// wilsonLowerRef is an algebraically-equivalent but differently-evaluated form of
// the Wilson score lower bound, used to cross-check errLowerBound's arithmetic
// (and its numerical stability) without re-deriving the same expression.
func wilsonLowerRef(p, z, n float64) float64 {
	z2 := z * z
	return (2*n*p + z2 - z*math.Sqrt(4*n*p*(1-p)+z2)) / (2 * (n + z2))
}

func TestErrLowerBoundWilson(t *testing.T) {
	const tol = 1e-9
	// tripZ is derived from the SLO at construction.
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: time.Second})

	for _, tc := range []struct {
		p, goodput float64
	}{
		{0.10, 50}, {0.30, 100}, {0.50, 100}, {0.75, 200}, {0.90, 500},
	} {
		l.errEWMA, l.goodput = tc.p, tc.goodput
		n := l.effectiveSamples()
		got := l.errLowerBound()
		want := wilsonLowerRef(tc.p, l.tripZ, n)
		if !approxEqual(got, want, tol) {
			t.Errorf("errLowerBound(p=%v, goodput=%v) = %v, want %v", tc.p, tc.goodput, got, want)
		}
		// A lower confidence bound must not exceed the point estimate.
		if got > tc.p+tol {
			t.Errorf("errLowerBound(p=%v) = %v exceeds point estimate", tc.p, got)
		}
	}
}

func TestErrLowerBoundNoGoodput(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: time.Second})
	l.goodput, l.errEWMA = 0, 0.42
	// With no capacity signal the bound degenerates to the point estimate.
	if got := l.errLowerBound(); got != 0.42 {
		t.Errorf("errLowerBound(goodput=0) = %v, want 0.42", got)
	}
}

func TestErrLowerBoundMonotonic(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: time.Second})
	l.goodput = 100
	prev := math.Inf(-1)
	for _, p := range []float64{0.05, 0.10, 0.20, 0.40, 0.60, 0.80} {
		l.errEWMA = p
		got := l.errLowerBound()
		if got < prev {
			t.Errorf("errLowerBound not monotonic in p: at p=%v got %v < previous %v", p, got, prev)
		}
		prev = got
	}
}
