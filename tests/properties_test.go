package tests

import (
	"math/rand"
	"testing"
	"time"

	"github.com/codemartial/levee"
)

func TestHealthyCapacityStepIsLearnedWithoutThrottling(t *testing.T) {
	slo := levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond}
	d := newDriver(t, levee.NewLevee(slo))
	start := time.Unix(0, 0)

	// Establish a 100 RPS, 10ms healthy regime (about one inflight).
	for i := 0; i < 300; i++ {
		d.arrive(start.Add(time.Duration(i)*10*time.Millisecond), 10*time.Millisecond, true, "healthy")
	}
	before := d.levee.Snapshot()
	stepAt := start.Add(3 * time.Second)
	// The backend genuinely scales to 2000 RPS at the same latency. This raises
	// healthy concurrency above the old surge onset, but completions prove it.
	for i := 0; i < 4000; i++ {
		d.arrive(stepAt.Add(time.Duration(i)*500*time.Microsecond), 10*time.Millisecond, true, "healthy")
	}
	d.flush()
	after := d.levee.Snapshot()

	if d.stats.transitions[levee.TriggerSurge] != 0 || d.stats.rejected != 0 {
		t.Fatalf("healthy capacity step was throttled: transitions=%+v rejected=%d", d.stats.transitions, d.stats.rejected)
	}
	if after.EstimatedCapacity <= before.EstimatedCapacity*5 {
		t.Fatalf("capacity did not adapt enough: before=%v after=%v", before.EstimatedCapacity, after.EstimatedCapacity)
	}
	t.Logf("healthy capacity step: estimate rose %.2f -> %.2f with no throttling or rejection", before.EstimatedCapacity, after.EstimatedCapacity)
}

func TestSurgeTripTimeFallsWithSeverity(t *testing.T) {
	tripStalled := func(multiplier int64) (time.Duration, int64) {
		l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: time.Second})
		start := time.Unix(0, 0)
		ts := warmHealthy(t, l, start, 100, 10*time.Millisecond, 10*time.Millisecond)
		onset := l.Snapshot().Surge.Onset
		for range onset * multiplier {
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("stalled priming Start: %v", err)
			}
		}
		for step := 1; step <= 1000; step++ {
			now := ts.Add(time.Duration(step) * time.Millisecond)
			sc, err := l.Start(now)
			if err == nil && sc.Trigger == levee.TriggerSurge {
				return now.Sub(ts), onset
			}
		}
		t.Fatalf("%dx stalled surge did not trip", multiplier)
		return 0, onset
	}

	twoX, onset := tripStalled(2)
	fourX, _ := tripStalled(4)
	if fourX >= twoX {
		t.Fatalf("4x stalled load tripped in %s, want faster than 2x at %s", fourX, twoX)
	}
	t.Logf("surge severity: onset=%d, 2x stalled load tripped in %s, 4x in %s", onset, twoX, fourX)
}

func TestSurgeStrainRelaxesAfterHealthyDrain(t *testing.T) {
	l := levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: time.Second})
	start := time.Unix(0, 0)
	ts := warmHealthy(t, l, start, 100, 10*time.Millisecond, 10*time.Millisecond)
	onset := l.Snapshot().Surge.Onset
	started := int(onset + 2)
	for range started {
		if _, err := l.Start(ts); err != nil {
			t.Fatalf("arming Start: %v", err)
		}
	}
	if s := l.Snapshot(); !s.Surge.Armed {
		t.Fatalf("surge did not arm: %+v", s)
	}

	for i := 0; i < started; i++ {
		done := ts.Add(time.Duration(i+1) * time.Millisecond)
		sc := l.Success(done, done.Sub(ts))
		if sc.Trigger == levee.TriggerSurge {
			t.Fatalf("healthy drain unexpectedly tripped surge protection at completion %d", i)
		}
	}
	s := l.Snapshot()
	if s.Surge.Armed || s.Surge.Strain != 0 || s.Inflight != 0 || s.State != levee.CLOSED {
		t.Fatalf("surge did not relax after healthy drain: %+v", s)
	}
	t.Logf("healthy drain relaxed armed strain to zero after %d completions", started)
}

func TestSeededAdversarialInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(20260711))
	d := newDriver(t, levee.NewLevee(levee.SLO{SuccessRate: 0.95, Timeout: 50 * time.Millisecond}))
	start := time.Unix(0, 0)
	ts := start

	const arrivals = 25_000
	for i := 0; i < arrivals; i++ {
		phase := (i / 2500) % 5
		interval := time.Duration(rng.Intn(10)+1) * time.Millisecond
		latency := time.Duration(rng.Intn(5)+1) * time.Millisecond
		failureProbability := 0.01
		switch phase {
		case 1:
			latency = time.Duration(rng.Intn(400)+100) * time.Millisecond
		case 2:
			failureProbability = 0.25
		case 3:
			interval = time.Duration(rng.Intn(3)+1) * time.Millisecond
			latency = time.Duration(rng.Intn(1000)+500) * time.Millisecond
			failureProbability = 0.10
		case 4:
			failureProbability = 0.80
		}

		// Correlate failures in short runs rather than drawing every sample
		// independently; retry-shaped phases also increase arrival density.
		bucket := (i / 7) % 100
		success := float64(bucket)/100 >= failureProbability
		if rng.Intn(20) == 0 {
			success = !success
		}
		d.arrive(ts, latency, success, "adversarial")
		ts = ts.Add(interval)
	}
	d.flush()
	d.checkSnapshot()

	if d.stats.admitted+d.stats.rejected != d.stats.attempted {
		t.Fatalf("accounting mismatch: %+v", d.stats)
	}
	if d.stats.succeeded+d.stats.failed != d.stats.admitted {
		t.Fatalf("completion mismatch: %+v", d.stats)
	}
	if len(d.stats.transitions) < 3 {
		t.Fatalf("adversarial trace did not exercise enough transition causes: %+v", d.stats.transitions)
	}
	t.Logf("seeded trace: attempted=%d admitted=%d rejected=%d transitions=%+v final=%+v", d.stats.attempted, d.stats.admitted, d.stats.rejected, d.stats.transitions, d.levee.Snapshot())
}
