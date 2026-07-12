package levee

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestStartsClosed(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	if got := l.State(); got != CLOSED {
		t.Fatalf("new breaker state = %v, want CLOSED", got)
	}

	start := time.Unix(100, 0)
	sc, err := l.Start(start)
	if err != nil {
		t.Fatalf("first Start returned error: %v", err)
	}
	if sc.State != CLOSED {
		t.Fatalf("first Start state = %v, want CLOSED", sc.State)
	}

	sc = l.Success(start.Add(20*time.Millisecond), 20*time.Millisecond)
	if sc.State != CLOSED {
		t.Fatalf("first Success state = %v, want CLOSED", sc.State)
	}
}

func TestClosedUncapped(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	// A healthy CLOSED breaker imposes no inflight limit: admit many requests
	// without completing them; none should be rejected. Guards the amd64 regression.
	start := time.Unix(100, 0)
	for i := range 1000 {
		ts := start.Add(time.Duration(i) * time.Microsecond)
		if _, err := l.Start(ts); err != nil {
			t.Fatalf("Start #%d on healthy CLOSED breaker rejected: %v", i, err)
		}
	}
}

func TestCall(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	called := 0
	sc, err := l.Call(func() error {
		called++
		return nil
	})
	if err != nil {
		t.Fatalf("successful Call returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("successful Call invoked function %d times, want 1", called)
	}
	if sc.State != CLOSED {
		t.Fatalf("successful Call state = %v, want CLOSED", sc.State)
	}

	wantErr := errors.New("upstream failed")
	sc, err = l.Call(func() error {
		called++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("failed Call error = %v, want %v", err, wantErr)
	}
	if called != 2 {
		t.Fatalf("failed Call invoked function count = %d, want 2", called)
	}
	if sc.State != CLOSED {
		t.Fatalf("isolated failed Call state = %v, want CLOSED", sc.State)
	}
}

func TestTrips(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	start := time.Unix(100, 0)
	rejected := false
	for i := range 200 {
		ts := start.Add(time.Duration(i) * 10 * time.Millisecond)
		sc, err := l.Start(ts)
		if err != nil {
			rejected = true
			if !errors.Is(err, ErrCircuitOpen) {
				t.Fatalf("Start rejection error = %v, want ErrCircuitOpen", err)
			}
			if sc.State == CLOSED {
				t.Fatalf("rejected Start reported CLOSED state")
			}
			break
		}

		l.Fail(ts.Add(20*time.Millisecond), 20*time.Millisecond)
	}

	if !rejected {
		t.Fatal("breaker never rejected after sustained failures")
	}
}

func TestRejectNeedsNoCompletion(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	rejectAt := tripWithFailures(t, l)
	for i := range 5 {
		_, err := l.Start(rejectAt.Add(time.Duration(i) * time.Millisecond))
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("rejected Start #%d error = %v, want ErrCircuitOpen", i, err)
		}
	}
}

func TestInvalidSLOPanics(t *testing.T) {
	tests := []struct {
		name string
		slo  SLO
	}{
		{
			name: "zero success rate",
			slo:  SLO{SuccessRate: 0, Timeout: time.Second},
		},
		{
			name: "perfect success rate",
			slo:  SLO{SuccessRate: 1, Timeout: time.Second},
		},
		{
			name: "nan success rate",
			slo:  SLO{SuccessRate: math.NaN(), Timeout: time.Second},
		},
		{
			name: "zero timeout",
			slo:  SLO{SuccessRate: 0.90},
		},
		{
			name: "negative timeout",
			slo:  SLO{SuccessRate: 0.90, Timeout: -time.Second},
		},
		{
			name: "success rate above one",
			slo:  SLO{SuccessRate: 1.1, Timeout: time.Second},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			_ = NewLevee(tt.slo)
		})
	}
}

func TestRecovers(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	tripAt := tripWithFailures(t, l)
	if l.State() == CLOSED {
		t.Fatal("breaker still CLOSED after sustained failures")
	}

	// Upstream is healthy again. Drive fast successful calls while event time
	// advances; the breaker should work its way back to CLOSED. We only touch
	// the public API and let it pick its own recovery path (OPEN -> HALF_OPEN
	// -> CLOSED or THROTTLED -> CLOSED).
	ts := tripAt
	recovered := false
	for range 4000 {
		ts = ts.Add(50 * time.Millisecond)
		if _, err := l.Start(ts); err == nil {
			l.Success(ts.Add(5*time.Millisecond), 5*time.Millisecond)
		}
		if l.State() == CLOSED {
			recovered = true
			break
		}
	}
	if !recovered {
		t.Fatalf("breaker did not return to CLOSED after sustained success; state = %v", l.State())
	}

	// Once recovered it admits work again.
	if _, err := l.Start(ts.Add(time.Second)); err != nil {
		t.Fatalf("recovered breaker rejected a request: %v", err)
	}
}

func TestOpenShortCircuits(t *testing.T) {
	// A long timeout keeps the breaker OPEN well beyond the test's wall-clock
	// duration, so Call (which stamps its own time.Now) is guaranteed to hit
	// the open circuit.
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Hour,
	})

	tripToOpen(t, l)

	called := false
	sc, err := l.Call(func() error {
		called = true
		return nil
	})
	if called {
		t.Fatal("Call invoked the function while the circuit was open")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Call on open circuit error = %v, want ErrCircuitOpen", err)
	}
	if sc.State == CLOSED {
		t.Fatal("Call on open circuit reported CLOSED state")
	}
}

func TestConcurrentUse(t *testing.T) {
	// Run under `go test -race` to exercise the breaker's locking. We assert
	// only black-box invariants: no panic/deadlock, a valid state, and that
	// the breaker is still usable afterwards.
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     50 * time.Millisecond,
	})
	flaky := errors.New("flaky upstream")

	const (
		workers = 16
		iters   = 500
	)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range iters {
				fail := (seed+i)%3 == 0
				_, _ = l.Call(func() error {
					if fail {
						return flaky
					}
					return nil
				})
			}
		}(w)
	}
	wg.Wait()

	switch l.State() {
	case CLOSED, OPEN, THROTTLED, HALF_OPEN:
	default:
		t.Fatalf("breaker in invalid state after concurrent use: %v", l.State())
	}

	// Still responsive: a rejection here is valid, a hang or panic is not.
	_, _ = l.Call(func() error { return nil })
}

// BenchmarkThroughput measures how many requests Levee can admit and complete
// per second on a single CPU. It drives the steady-state healthy path
// (Start -> Success on a CLOSED breaker) using a synthetic, monotonically
// advancing clock, so the figure reflects the breaker's own overhead rather
// than the cost of reading the OS clock.
//
// Pin it to one CPU and read the reqs/sec metric:
//
//	go test -run '^$' -bench BenchmarkThroughput -cpu 1
func BenchmarkThroughput(b *testing.B) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	ts := time.Unix(0, 0)
	const step = 100 * time.Microsecond // event-time spacing between requests
	const dur = 5 * time.Millisecond    // simulated call latency

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		ts = ts.Add(step)
		if _, err := l.Start(ts); err != nil {
			b.Fatalf("healthy CLOSED breaker rejected request at iter %d: %v", i, err)
		}
		l.Success(ts.Add(dur), dur)
	}
	b.StopTimer()

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "reqs/sec")
}

// BenchmarkContended measures admission throughput when many goroutines hammer
// the same breaker, exposing contention on the single internal mutex. Each
// goroutine drives the healthy CLOSED path with its own local synthetic clock,
// so the figure reflects lock contention rather than time.Now or scheduling.
// Compare across -cpu values to see whether throughput scales with cores:
//
//	go test -run '^$' -bench BenchmarkContended -cpu 1,2,4,8
func BenchmarkContended(b *testing.B) {
	l := NewLevee(SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	})

	const step = 100 * time.Microsecond // event-time spacing between requests
	const dur = 5 * time.Millisecond    // simulated call latency

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		ts := time.Unix(0, 0)
		for pb.Next() {
			ts = ts.Add(step)
			if _, err := l.Start(ts); err != nil {
				b.Errorf("healthy CLOSED breaker rejected request: %v", err)
				return
			}
			l.Success(ts.Add(dur), dur)
		}
	})

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "reqs/sec")
}

func tripWithFailures(t *testing.T, l *Levee) time.Time {
	t.Helper()

	start := time.Unix(200, 0)
	for i := range 200 {
		ts := start.Add(time.Duration(i) * 10 * time.Millisecond)
		_, err := l.Start(ts)
		if err != nil {
			return ts
		}
		l.Fail(ts.Add(20*time.Millisecond), 20*time.Millisecond)
	}

	t.Fatal("breaker never rejected after sustained failures")
	return time.Time{}
}

// tripToOpen drives the breaker to the OPEN state by failing every admitted
// request, anchored at the current wall clock so the resulting cooldown window
// overlaps a subsequent Call.
func tripToOpen(t *testing.T, l *Levee) {
	t.Helper()

	start := time.Now()
	for i := range 5000 {
		ts := start.Add(time.Duration(i) * 20 * time.Millisecond)
		if _, err := l.Start(ts); err == nil {
			l.Fail(ts.Add(5*time.Millisecond), 5*time.Millisecond)
		}
		if l.State() == OPEN {
			return
		}
	}
	t.Fatalf("breaker never reached OPEN; state = %v", l.State())
}

// onFastPath reports whether the breaker is currently in its lock-free admission
// mode: uncapped, which can only hold while CLOSED.
func onFastPath(l *Levee) bool {
	return !l.capped.Load()
}

// TestFastPathConcurrentAccounting hammers a healthy CLOSED breaker from many
// goroutines, exercising the lock-free admission path, and verifies the in-flight
// counter balances back to zero with no lost or double counts. Run under -race to
// confirm the atomic accounting is data-race free.
func TestFastPathConcurrentAccounting(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.99, Timeout: time.Second})
	if !onFastPath(l) {
		t.Fatal("fresh CLOSED breaker should start on the lock-free fast path")
	}

	const (
		workers = 32
		iters   = 2000
	)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ts := time.Unix(int64(seed), 0)
			for range iters {
				ts = ts.Add(time.Microsecond)
				if _, err := l.Start(ts); err != nil {
					t.Errorf("healthy CLOSED breaker rejected: %v", err)
					return
				}
				l.Success(ts, time.Millisecond)
			}
		}(w)
	}
	wg.Wait()

	if got := l.inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d after balanced Start/Success pairs, want 0", got)
	}
	if l.State() != CLOSED {
		t.Fatalf("state = %v after all-success load, want CLOSED", l.State())
	}
}

// TestFastPathTogglesWithState checks that the lock-free mode is engaged only
// while the breaker is CLOSED and uncapped, and is cleared the moment it trips so
// admission falls back to the mutex-guarded, limit-enforcing slow path.
func TestFastPathTogglesWithState(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
	if !onFastPath(l) {
		t.Fatal("healthy CLOSED breaker should be on the fast path")
	}

	// Drive it out of CLOSED with sustained failures.
	tripWithFailures(t, l)
	if l.State() == CLOSED {
		t.Fatal("breaker still CLOSED after sustained failures")
	}
	if onFastPath(l) {
		t.Fatalf("fast path still engaged in state %v; admission would bypass the limit", l.State())
	}
}

// TestFastPathConcurrentWithFailures mixes successes and failures across many
// goroutines so the breaker trips, throttles, and recovers while the lock-free
// gate flips underneath. The invariant under test is purely the accounting: every
// admitted call is completed exactly once, so inflight must end at zero.
func TestFastPathConcurrentWithFailures(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: 20 * time.Millisecond})

	const (
		workers = 16
		iters   = 3000
	)
	var wg sync.WaitGroup
	base := time.Unix(0, 0)
	for w := range workers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ts := base.Add(time.Duration(seed) * time.Millisecond)
			for i := range iters {
				ts = ts.Add(time.Microsecond)
				if _, err := l.Start(ts); err != nil {
					continue // rejected: nothing to complete
				}
				if (seed+i)%4 == 0 {
					l.Fail(ts, time.Millisecond)
				} else {
					l.Success(ts, time.Millisecond)
				}
			}
		}(w)
	}
	wg.Wait()

	if got := l.inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d after every admitted call was completed, want 0", got)
	}
}

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

			if l.inflight.Load() != int64(outstanding) {
				t.Fatalf("inflight = %d, want %d outstanding", l.inflight.Load(), outstanding)
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

func TestTriggerString(t *testing.T) {
	tests := []struct {
		trigger Trigger
		want    string
	}{
		{TriggerNone, "NONE"},
		{TriggerFailureRate, "FAILURE_RATE"},
		{TriggerConsecutiveFailures, "CONSECUTIVE_FAILURES"},
		{TriggerSurge, "SURGE"},
		{TriggerMinLimitFailureRate, "MIN_LIMIT_FAILURE_RATE"},
		{TriggerCooldownExpired, "COOLDOWN_EXPIRED"},
		{TriggerRecovered, "RECOVERED"},
		{Trigger(255), "Trigger(255)"},
	}
	for _, tt := range tests {
		if got := tt.trigger.String(); got != tt.want {
			t.Errorf("Trigger(%d).String() = %q, want %q", tt.trigger, got, tt.want)
		}
	}
}

func TestSteadyCallsHaveNoTrigger(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
	ts := time.Unix(0, 0)

	sc, err := l.Start(ts)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Trigger != TriggerNone {
		t.Fatalf("steady Start trigger = %v, want NONE", sc.Trigger)
	}
	sc = l.Success(ts.Add(time.Millisecond), time.Millisecond)
	if sc.Trigger != TriggerNone {
		t.Fatalf("steady Success trigger = %v, want NONE", sc.Trigger)
	}
}

func TestTransitionTriggers(t *testing.T) {
	t.Run("consecutive failures", func(t *testing.T) {
		l := NewLevee(SLO{SuccessRate: 0.99, Timeout: time.Second})
		downstreamErr := errors.New("downstream failed")
		for i := 0; i < 20; i++ {
			sc, err := l.Call(func() error { return downstreamErr })
			if !errors.Is(err, downstreamErr) {
				t.Fatalf("Call error = %v, want downstream error", err)
			}
			if sc.Trigger != TriggerNone {
				if sc.Trigger != TriggerConsecutiveFailures {
					t.Fatalf("transition trigger = %v, want %v", sc.Trigger, TriggerConsecutiveFailures)
				}
				s := l.Snapshot()
				if s.State != THROTTLED || s.Trigger != TriggerConsecutiveFailures || !s.Capped || s.Limit < 1 {
					t.Fatalf("consecutive-failure snapshot = %+v", s)
				}
				return
			}
		}
		t.Fatal("consecutive failures did not throttle")
	})

	t.Run("statistical failure evidence", func(t *testing.T) {
		l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
		ts := time.Unix(0, 0)
		for range warmupSamples + 10 {
			ts = ts.Add(10 * time.Millisecond)
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("warmup Start: %v", err)
			}
			l.Success(ts.Add(time.Millisecond), time.Millisecond)
		}

		// Alternate outcomes so consecutive failures cannot be the cause.
		for i := 0; i < 5000; i++ {
			ts = ts.Add(10 * time.Millisecond)
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("failure Start: %v", err)
			}
			sc := l.Fail(ts.Add(time.Millisecond), time.Millisecond)
			if sc.Trigger != TriggerNone {
				if sc.Trigger != TriggerFailureRate {
					t.Fatalf("transition trigger = %v, want %v", sc.Trigger, TriggerFailureRate)
				}
				if s := l.Snapshot(); s.State != THROTTLED || s.Trigger != TriggerFailureRate || !s.Capped {
					t.Fatalf("failure-rate snapshot = %+v", s)
				}
				return
			}

			ts = ts.Add(10 * time.Millisecond)
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("success Start: %v", err)
			}
			if sc := l.Success(ts.Add(time.Millisecond), time.Millisecond); sc.Trigger != TriggerNone {
				t.Fatalf("success transition trigger = %v", sc.Trigger)
			}
		}
		t.Fatal("statistical failure evidence did not throttle")
	})

	t.Run("surge strain", func(t *testing.T) {
		l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
		l.samples = warmupSamples
		l.goodput = 100
		l.avgLatency = 0.01
		l.publishSurgeLimit()
		onset := l.surgeLimit.Load()
		if onset <= 0 {
			t.Fatal("surge onset was not published")
		}

		ts := time.Unix(0, 0)
		for range onset + 1 {
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("priming Start: %v", err)
			}
		}
		armed := l.Snapshot()
		if armed.State != CLOSED || armed.Capped || !armed.Surge.Armed || armed.Surge.Onset != onset {
			t.Fatalf("armed surge snapshot = %+v", armed)
		}
		sc, err := l.Start(ts.Add(2 * time.Second))
		if err != nil {
			t.Fatalf("surge Start: %v", err)
		}
		if sc.Trigger != TriggerSurge || sc.State != THROTTLED {
			t.Fatalf("surge change = %+v, want THROTTLED/SURGE", sc)
		}
		if s := l.Snapshot(); s.Trigger != TriggerSurge || !s.Capped || s.Surge.Armed || s.Surge.Strain < 1 {
			t.Fatalf("tripped surge snapshot = %+v", s)
		}
	})

	t.Run("minimum limit failure rate and cooldown", func(t *testing.T) {
		l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
		base := time.Unix(0, 0)
		l.transition(THROTTLED, TriggerFailureRate, base)
		l.capped.Store(true)
		l.inflightLimit = minInflightLimit
		l.avgLatency = 0.01
		l.lastEvalTS = base
		l.evalFailures = 1

		sc, err := l.Start(base.Add(time.Second))
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("opening Start error = %v, want ErrCircuitOpen", err)
		}
		if sc.Trigger != TriggerMinLimitFailureRate || sc.State != OPEN {
			t.Fatalf("opening change = %+v, want OPEN/MIN_LIMIT_FAILURE_RATE", sc)
		}
		if s := l.Snapshot(); s.State != OPEN || s.Trigger != TriggerMinLimitFailureRate || !s.Capped {
			t.Fatalf("open snapshot = %+v", s)
		}

		sc, err = l.Start(base.Add(2 * time.Second))
		if err != nil {
			t.Fatalf("recovery probe Start: %v", err)
		}
		if sc.Trigger != TriggerCooldownExpired || sc.State != HALF_OPEN {
			t.Fatalf("probe change = %+v, want HALF_OPEN/COOLDOWN_EXPIRED", sc)
		}
		if s := l.Snapshot(); s.State != HALF_OPEN || s.Trigger != TriggerCooldownExpired || s.Limit != 1 {
			t.Fatalf("half-open snapshot = %+v", s)
		}
		l.Success(base.Add(2*time.Second+time.Millisecond), time.Millisecond)
	})

	t.Run("healthy recovery", func(t *testing.T) {
		l := NewLevee(SLO{SuccessRate: 0.90, Timeout: time.Second})
		base := time.Unix(0, 0)
		l.transition(THROTTLED, TriggerFailureRate, base)
		l.capped.Store(true)
		l.inflightLimit = 2
		l.avgLatency = 0.01
		l.lastEvalTS = base

		start := base.Add(4 * time.Second)
		if sc, err := l.Start(start); err != nil || sc.Trigger != TriggerNone {
			t.Fatalf("Start = %+v, %v", sc, err)
		}
		sc := l.Success(start.Add(time.Millisecond), time.Millisecond)
		if sc.Trigger != TriggerRecovered || sc.State != CLOSED {
			t.Fatalf("recovery change = %+v, want CLOSED/RECOVERED", sc)
		}
		if s := l.Snapshot(); s.Trigger != TriggerRecovered || s.State != CLOSED || !s.Capped {
			t.Fatalf("recovered snapshot = %+v", s)
		}

		if sc, err := l.Start(start.Add(2 * time.Millisecond)); err != nil || sc.Trigger != TriggerNone {
			t.Fatalf("steady post-recovery Start = %+v, %v", sc, err)
		}
		if got := l.Snapshot().Trigger; got != TriggerRecovered {
			t.Fatalf("steady call replaced retained trigger with %v", got)
		}
	})
}

func TestSnapshot(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: time.Second})
	initial := l.Snapshot()
	if initial.State != CLOSED || initial.Trigger != TriggerNone || initial.Inflight != 0 || initial.Capped || initial.Limit != 0 {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	if initial.EstimatedCapacity != 0 || initial.ErrorRate != 0 || initial.ErrorLowerBound != 0 {
		t.Fatalf("initial estimates = %+v", initial)
	}
	if initial.Surge != (SurgeSnapshot{}) {
		t.Fatalf("initial surge = %+v", initial.Surge)
	}

	base := time.Unix(0, 0)
	l.transition(THROTTLED, TriggerFailureRate, base)
	l.capped.Store(true)
	l.inflight.Store(2)
	l.inflightLimit = 3.2
	l.goodput = 100
	l.avgLatency = 0.04
	l.errEWMA = 0.20
	l.samples = warmupSamples
	l.surgeArmed = true
	l.surgeLatSnap = 0.04
	l.surgeStrain = 0.40
	l.surgeLimit.Store(17)
	l.surgeProven = 1.5
	wantLowerBound := l.errLowerBound()

	got := l.Snapshot()
	if got.State != THROTTLED || got.Trigger != TriggerFailureRate {
		t.Fatalf("snapshot state/trigger = %v/%v", got.State, got.Trigger)
	}
	if got.Inflight != 2 || !got.Capped || got.Limit != 4 {
		t.Fatalf("snapshot admission = inflight %d capped %v limit %d", got.Inflight, got.Capped, got.Limit)
	}
	if got.EstimatedCapacity != 4 || got.ErrorRate != 0.20 || got.ErrorLowerBound != wantLowerBound {
		t.Fatalf("snapshot estimates = capacity %v error %v lower %v", got.EstimatedCapacity, got.ErrorRate, got.ErrorLowerBound)
	}
	if !got.Surge.Armed || got.Surge.Onset != 17 || got.Surge.Strain != 2 || got.Surge.ProvenExcess != 1.5 {
		t.Fatalf("snapshot surge = %+v", got.Surge)
	}
	if again := l.Snapshot(); again != got {
		t.Fatalf("Snapshot mutated state: first %+v, second %+v", got, again)
	}
}

func TestSnapshotWarmHealthy(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.95, Timeout: time.Second})
	ts := time.Unix(0, 0)
	for range warmupSamples + 10 {
		ts = ts.Add(10 * time.Millisecond)
		if _, err := l.Start(ts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		l.Success(ts.Add(5*time.Millisecond), 5*time.Millisecond)
	}

	s := l.Snapshot()
	if s.State != CLOSED || s.Trigger != TriggerNone || s.Capped || s.Limit != 0 {
		t.Fatalf("warm healthy state = %+v", s)
	}
	if s.EstimatedCapacity <= 0 || s.ErrorRate != 0 || math.Abs(s.ErrorLowerBound) > 1e-15 {
		t.Fatalf("warm healthy estimates = %+v", s)
	}
	if s.Surge.Armed || s.Surge.Onset <= 0 || s.Surge.Strain != 0 {
		t.Fatalf("warm healthy surge = %+v", s.Surge)
	}
}

func TestSnapshotConcurrentUse(t *testing.T) {
	l := NewLevee(SLO{SuccessRate: 0.90, Timeout: 10 * time.Millisecond})
	const workers = 8
	const iterations = 500

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range iterations {
				_, _ = l.Call(func() error {
					if (seed+i)%7 == 0 {
						return errors.New("downstream failure")
					}
					return nil
				})
			}
		}(w)
	}

	for range iterations {
		s := l.Snapshot()
		if s.State > HALF_OPEN || s.Trigger > TriggerRecovered {
			t.Fatalf("invalid snapshot enum values: %+v", s)
		}
		for name, value := range map[string]float64{
			"capacity":    s.EstimatedCapacity,
			"error rate":  s.ErrorRate,
			"error bound": s.ErrorLowerBound,
			"strain":      s.Surge.Strain,
			"proven":      s.Surge.ProvenExcess,
		} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatalf("snapshot %s is not finite: %v", name, value)
			}
		}
		if !s.Capped && s.Limit != 0 {
			t.Fatalf("uncapped snapshot has limit %d", s.Limit)
		}
	}
	wg.Wait()
}

func TestLeveeSize(t *testing.T) {
	const maxSize = 320
	if got := unsafe.Sizeof(Levee{}); got > maxSize {
		t.Fatalf("Levee size = %d bytes, want <= %d", got, maxSize)
	} else {
		t.Logf("Levee size: %d bytes", got)
	}
}
