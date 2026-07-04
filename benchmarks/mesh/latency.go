package mesh

import "math"

// defaultTimeoutMS is the root request budget; also caps latency samples.
const defaultTimeoutMS = 1500.0

// latencyProfile mirrors backend.LatencyProfile: y = a + b*x/(1-c*x) for
// x in [0, 0.99], linear tail to timeout. Local copy: params are unexported upstream.
type latencyProfile struct {
	p50, p99, timeoutMS float64
	a, b, c             float64
}

func newLatencyProfile(p50, p99, timeout float64) latencyProfile {
	lp := latencyProfile{p50: p50, p99: p99, timeoutMS: timeout}
	lp.a = p50 / 2
	lp.c = (1.49*p50 - p99) / (0.99 * (p50 - p99))
	lp.b = p50 * (1 - 0.5*lp.c)
	return lp
}

// sample maps a uniform draw x in [0,1) to a latency in milliseconds.
// Randomness is supplied by the caller so draws stay candidate-independent.
func (lp latencyProfile) sample(x float64) float64 {
	var ms float64
	if x <= 0.99 {
		ms = lp.a + lp.b*x/(1-lp.c*x)
	} else {
		slope := (lp.timeoutMS - lp.p99) / 0.01
		ms = lp.p99 + slope*(x-0.99)
	}
	return math.Min(math.Max(ms, 0), lp.timeoutMS)
}

// meanMS integrates the piecewise distribution analytically.
func (lp latencyProfile) meanMS() float64 {
	var rationalIntegral float64
	if math.Abs(lp.c) < 1e-10 {
		rationalIntegral = lp.a*0.99 + lp.b*0.99*0.99/2.0
	} else {
		rationalIntegral = lp.a*0.99 + lp.b*(-0.99/lp.c-math.Log(1-0.99*lp.c)/(lp.c*lp.c))
	}
	slope := (lp.timeoutMS - lp.p99) / 0.01
	tailIntegral := lp.p99*0.01 + slope*0.00005
	return rationalIntegral + tailIntegral
}
