package levee

import (
	"sync"
	"testing"
	"time"
)

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
