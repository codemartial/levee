package levee

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
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

	// A healthy CLOSED breaker imposes no in-flight limit. Admit many requests
	// without completing them; none should be rejected for capacity. Regression
	// for the unlimited-limit sentinel: int64(math.Ceil(MaxFloat64)) overflows to
	// MinInt64 on amd64, which made the breaker reject every request.
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

func TestSaveRestore(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	}
	l := NewLevee(slo)
	start := time.Unix(100, 0)

	for i := range 10 {
		ts := start.Add(time.Duration(i) * time.Millisecond)
		if _, err := l.Start(ts); err != nil {
			t.Fatalf("Start before save returned error: %v", err)
		}
		l.Success(ts.Add(10*time.Millisecond), 10*time.Millisecond)
	}

	saved, err := l.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error: %v", err)
	}

	restored := RestoreState(saved)
	if restored.State() != l.State() {
		t.Fatalf("restored state = %v, want %v", restored.State(), l.State())
	}

	ts := start.Add(time.Second)
	if _, err := restored.Start(ts); err != nil {
		t.Fatalf("restored breaker Start returned error: %v", err)
	}
	restored.Success(ts.Add(10*time.Millisecond), 10*time.Millisecond)
}

func TestRestoreDropsInflight(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.90,
		Timeout:     time.Second,
	}
	now := time.Unix(100, 0)

	l := NewLevee(slo)
	l.mu.Lock()
	l.state = THROTTLED
	l.stateEnteredAt = now
	l.lastEvalTS = now
	l.inflight = 1
	l.inflightLimit = 1
	l.mu.Unlock()

	saved, err := l.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error: %v", err)
	}
	if saved.Inflight != 1 {
		t.Fatalf("expected saved in-flight count to reflect runtime state, got %d", saved.Inflight)
	}

	restored := RestoreState(saved)
	if restored.inflight != 0 {
		t.Fatalf("restored in-flight count = %d, want 0", restored.inflight)
	}

	if _, err := restored.Start(now.Add(time.Second)); err != nil {
		t.Fatalf("restored breaker should admit a request after dropping stale in-flight count: %v", err)
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

func TestRestoreNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()

	_ = RestoreState(nil)
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
