package levee

import (
	"math"
	"testing"
	"time"
)

// FuzzAdmission is the fuzzing analogue of TestStateMachineInvariants: it lets the
// fuzzer choose the SLO and an arbitrary interleaving of admissions and
// completions, and asserts the same invariants after every step. It targets the
// float-to-int admission path (int64(math.Ceil(inflightLimit))) that regressed
// across architectures, exploring odd timings and orderings a fixed test would not.
func FuzzAdmission(f *testing.F) {
	f.Add(uint16(0), []byte{0, 1, 2, 0, 1, 2, 0, 0, 2, 1})
	f.Add(uint16(3), []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(uint16(7), []byte{2, 2, 2, 2, 2, 0, 2, 0, 2, 0, 2, 0})

	slos := []SLO{
		{SuccessRate: 0.50, Timeout: time.Millisecond},
		{SuccessRate: 0.90, Timeout: 10 * time.Millisecond},
		{SuccessRate: 0.95, Timeout: 50 * time.Millisecond},
		{SuccessRate: 0.99, Timeout: time.Second},
		{SuccessRate: 0.999, Timeout: time.Hour},
	}

	f.Fuzz(func(t *testing.T, sloSel uint16, ops []byte) {
		l := NewLevee(slos[int(sloSel)%len(slos)])
		ts := time.Unix(0, 0)
		outstanding := 0

		for _, b := range ops {
			ts = ts.Add(time.Duration(b%7+1) * time.Millisecond)

			if b%3 == 0 {
				before := l.State()
				_, err := l.Start(ts)
				if !legalTransitions[before][l.State()] {
					t.Fatalf("illegal transition %v -> %v on Start", before, l.State())
				}
				if err == nil {
					outstanding++
				}
			} else {
				if outstanding == 0 {
					continue
				}
				before := l.State()
				dur := time.Duration(b%13+1) * time.Millisecond
				if b%2 == 0 {
					l.Fail(ts, dur)
				} else {
					l.Success(ts, dur)
				}
				if !legalTransitions[before][l.State()] {
					t.Fatalf("illegal transition %v -> %v on completion", before, l.State())
				}
				outstanding--
			}

			if l.inflight != int64(outstanding) {
				t.Fatalf("inflight = %d, want %d outstanding", l.inflight, outstanding)
			}
			if (l.state == THROTTLED || l.state == HALF_OPEN) && l.inflightLimit < minInflightLimit {
				t.Fatalf("inflightLimit %v below floor in state %v", l.inflightLimit, l.state)
			}
		}
	})
}

func FuzzInvNormCDF(f *testing.F) {
	for _, p := range []float64{0.5, 0.975, 0.0, 1.0, -1.0, 2.0, 1e-300, 0.9999999} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p float64) {
		got := invNormCDF(p)
		switch {
		case math.IsNaN(p):
			// NaN in, NaN out is acceptable; nothing to assert.
		case p <= 0:
			if !math.IsInf(got, -1) {
				t.Fatalf("invNormCDF(%v) = %v, want -Inf", p, got)
			}
		case p >= 1:
			if !math.IsInf(got, 1) {
				t.Fatalf("invNormCDF(%v) = %v, want +Inf", p, got)
			}
		default:
			// In the open interval the result must never be NaN. It may reach
			// +/-Inf only in the extreme tails, and its sign must track p<>0.5.
			if math.IsNaN(got) {
				t.Fatalf("invNormCDF(%v) = NaN", p)
			}
			if p < 0.5 && got > 0 {
				t.Fatalf("invNormCDF(%v) = %v, want <= 0 for p < 0.5", p, got)
			}
			if p > 0.5 && got < 0 {
				t.Fatalf("invNormCDF(%v) = %v, want >= 0 for p > 0.5", p, got)
			}
		}
	})
}

func fuzzClampOpenUnit(x float64) float64 {
	x = math.Abs(x)
	x -= math.Floor(x) // fractional part, in [0, 1)
	if x < 0.01 {
		x = 0.01
	}
	if x > 0.99 {
		x = 0.99
	}
	return x
}

func fuzzClamp01(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}

// FuzzWilson exercises the Wilson lower-bound arithmetic across the full domain of
// its inputs, including extremes (error rate at 0 or 1, tiny and huge goodput). A
// confidence bound must never be NaN/Inf and must not exceed the point estimate.
func FuzzWilson(f *testing.F) {
	f.Add(0.95, 0.10, 100.0)
	f.Add(0.99, 0.99, 1.0)
	f.Add(0.50, 0.0, 0.0)

	f.Fuzz(func(t *testing.T, successRate, errEWMA, goodput float64) {
		if math.IsNaN(successRate) || math.IsNaN(errEWMA) || math.IsNaN(goodput) || math.IsInf(goodput, 0) {
			return
		}
		successRate = fuzzClampOpenUnit(successRate)
		errEWMA = fuzzClamp01(errEWMA)
		goodput = math.Abs(goodput)

		l := NewLevee(SLO{SuccessRate: successRate, Timeout: time.Second})
		l.errEWMA, l.goodput = errEWMA, goodput

		got := l.errLowerBound()
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("errLowerBound(rate=%v, err=%v, goodput=%v) = %v, want finite",
				successRate, errEWMA, goodput, got)
		}
		if got > errEWMA+1e-9 {
			t.Fatalf("errLowerBound = %v exceeds point estimate %v", got, errEWMA)
		}
	})
}
