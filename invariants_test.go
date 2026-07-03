package levee

import (
	"math/rand"
	"testing"
	"time"
)

// legalTransitions encodes every state edge the breaker is allowed to take when
// observed across a single public call. CLOSED only tightens to THROTTLED; OPEN
// only re-probes via HALF_OPEN; the limited states can recover to CLOSED or trip
// to OPEN. Any edge outside this set is a state-machine bug.
var legalTransitions = map[State]map[State]bool{
	CLOSED:    {CLOSED: true, THROTTLED: true},
	THROTTLED: {THROTTLED: true, CLOSED: true, OPEN: true},
	HALF_OPEN: {HALF_OPEN: true, CLOSED: true, OPEN: true},
	OPEN:      {OPEN: true, HALF_OPEN: true},
}

func checkInvariants(t *testing.T, l *Levee, outstanding int, step int) {
	t.Helper()
	// The breaker's inflight counter must agree with the number of calls we have
	// admitted but not yet completed. This subsumes "never negative" and
	// "a rejected Start must not increment inflight".
	if l.inflight.Load() != int64(outstanding) {
		t.Fatalf("step %d: inflight = %d, but %d calls are outstanding", step, l.inflight.Load(), outstanding)
	}
	// While admission is capped, the limit must never fall below the floor.
	if (l.state == THROTTLED || l.state == HALF_OPEN) && l.inflightLimit < minInflightLimit {
		t.Fatalf("step %d: inflightLimit = %v below floor %v in state %v",
			step, l.inflightLimit, minInflightLimit, l.state)
	}
}

// TestStateMachineInvariants drives a long, deterministic, pseudo-random mix of
// admissions and completions with a synthetic clock, building up real inflight
// concurrency, and asserts the core invariants after every operation: legal state
// transitions, a consistent inflight count, and the limit floor. The failure
// probability is swept so the breaker actually trips, throttles, opens, and
// recovers over the run.
func TestStateMachineInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: 50 * time.Millisecond})

	ts := time.Unix(0, 0)
	var outstanding []time.Time // start timestamps of admitted, not-yet-completed calls
	seenState := map[State]bool{}

	const steps = 200_000
	for step := range steps {
		ts = ts.Add(time.Duration(rng.Intn(5)+1) * time.Millisecond)

		// Sweep the failure rate in slow waves so we exercise trip and recovery.
		failProb := 0.02
		if (step/20_000)%2 == 0 {
			failProb = 0.6
		}

		if len(outstanding) == 0 || rng.Intn(2) == 0 {
			// Attempt to admit a new call.
			before := l.State()
			_, err := l.Start(ts)
			after := l.State()
			if !legalTransitions[before][after] {
				t.Fatalf("step %d: illegal transition %v -> %v on Start", step, before, after)
			}
			if err == nil {
				outstanding = append(outstanding, ts)
			}
			seenState[after] = true
			checkInvariants(t, l, len(outstanding), step)
		} else {
			// Complete a random outstanding call.
			i := rng.Intn(len(outstanding))
			startTS := outstanding[i]
			outstanding = append(outstanding[:i], outstanding[i+1:]...)

			done := ts
			if done.Before(startTS) {
				done = startTS
			}
			dur := done.Sub(startTS) + time.Millisecond

			before := l.State()
			if rng.Float64() < failProb {
				l.Fail(done, dur)
			} else {
				l.Success(done, dur)
			}
			after := l.State()
			if !legalTransitions[before][after] {
				t.Fatalf("step %d: illegal transition %v -> %v on completion", step, before, after)
			}
			seenState[after] = true
			checkInvariants(t, l, len(outstanding), step)
		}
	}

	// Sanity check that the sweep actually visited the interesting states, so the
	// invariants above were tested against real transitions and not just CLOSED.
	for _, s := range []State{CLOSED, THROTTLED, OPEN} {
		if !seenState[s] {
			t.Errorf("sweep never reached state %v; the test is not exercising the breaker", s)
		}
	}
}

// TestRejectionPreservesInflight focuses the "rejected admission must not change
// inflight" invariant: it saturates a throttled breaker and confirms that every
// rejected Start leaves the inflight counter untouched.
func TestRejectionPreservesInflight(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: 50 * time.Millisecond})
	ts := tripWithFailures(t, l) // drives the breaker out of CLOSED

	rejected := 0
	for i := range 1000 {
		ts = ts.Add(time.Millisecond)
		before := l.inflight.Load()
		if _, err := l.Start(ts); err != nil {
			rejected++
			if l.inflight.Load() != before {
				t.Fatalf("iter %d: rejected Start changed inflight: %d -> %d", i, before, l.inflight.Load())
			}
		} else {
			// Admitted: complete it so we keep cycling against the limit.
			if l.inflight.Load() != before+1 {
				t.Fatalf("iter %d: admitted Start did not increment inflight: %d -> %d", i, before, l.inflight.Load())
			}
			l.Fail(ts.Add(time.Millisecond), time.Millisecond)
		}
	}
	if rejected == 0 {
		t.Fatal("expected some rejections from a saturated/limited breaker, got none")
	}
}
