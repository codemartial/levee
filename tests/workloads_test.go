package tests

import (
	"testing"
	"time"

	"github.com/codemartial/levee"
)

func TestLowRateLearningAndColdOutage(t *testing.T) {
	slo := levee.SLO{SuccessRate: 0.95, Timeout: time.Second}
	start := time.Unix(0, 0)

	healthy := levee.NewLevee(slo)
	ts := start
	for range 200 {
		sc, err := healthy.Start(ts)
		if err != nil {
			t.Fatalf("healthy 0.1 RPS request rejected: %v", err)
		}
		if sc.Trigger != levee.TriggerNone {
			t.Fatalf("healthy 0.1 RPS request transitioned: %+v", sc)
		}
		healthy.Success(ts.Add(20*time.Millisecond), 20*time.Millisecond)
		ts = ts.Add(10 * time.Second)
	}
	if s := healthy.Snapshot(); s.State != levee.CLOSED || s.ErrorRate != 0 {
		t.Fatalf("healthy low-rate snapshot = %+v", s)
	}

	measureOutage := func(interval time.Duration) (int, time.Duration, levee.Trigger) {
		l := levee.NewLevee(slo)
		ts := start
		for attempt := 1; attempt <= 20; attempt++ {
			if _, err := l.Start(ts); err != nil {
				t.Fatalf("outage request %d rejected before throttling transition: %v", attempt, err)
			}
			sc := l.Fail(ts.Add(20*time.Millisecond), 20*time.Millisecond)
			if sc.Trigger != levee.TriggerNone {
				return attempt, ts.Sub(start), sc.Trigger
			}
			ts = ts.Add(interval)
		}
		t.Fatal("cold outage did not transition")
		return 0, 0, levee.TriggerNone
	}

	lowAttempts, lowElapsed, lowTrigger := measureOutage(10 * time.Second)
	highAttempts, highElapsed, highTrigger := measureOutage(100 * time.Millisecond)
	if lowAttempts != highAttempts || lowAttempts != 5 {
		t.Fatalf("cold outage attempts: low=%d high=%d, want both 5", lowAttempts, highAttempts)
	}
	if lowTrigger != levee.TriggerConsecutiveFailures || highTrigger != levee.TriggerConsecutiveFailures {
		t.Fatalf("cold outage triggers: low=%s high=%s", lowTrigger, highTrigger)
	}
	if lowElapsed <= highElapsed {
		t.Fatalf("low-rate detection %s should take longer than high-rate detection %s", lowElapsed, highElapsed)
	}
	t.Logf("cold outage: %d failures to throttle; 0.1 RPS took %s, 10 RPS took %s", lowAttempts, lowElapsed, highElapsed)
}

func TestMixedRequestClassesShouldBeSplit(t *testing.T) {
	slo := levee.SLO{SuccessRate: 0.95, Timeout: 100 * time.Millisecond}
	shared := newDriver(t, levee.NewLevee(slo))
	fast := newDriver(t, levee.NewLevee(slo))
	slow := newDriver(t, levee.NewLevee(slo))

	start := time.Unix(0, 0)
	const warmTicks = 300
	const degradedTicks = 2500
	for tick := 0; tick < warmTicks+degradedTicks; tick++ {
		ts := start.Add(time.Duration(tick) * 10 * time.Millisecond)
		shared.arrive(ts, 5*time.Millisecond, true, "fast")
		fast.arrive(ts, 5*time.Millisecond, true, "fast")

		if tick%5 == 0 {
			slowHealthy := tick < warmTicks
			shared.arrive(ts.Add(time.Microsecond), 50*time.Millisecond, slowHealthy, "slow")
			slow.arrive(ts.Add(time.Microsecond), 50*time.Millisecond, slowHealthy, "slow")
		}
	}
	shared.flush()
	fast.flush()
	slow.flush()

	if fast.stats.rejected != 0 || fast.levee.State() != levee.CLOSED {
		t.Fatalf("isolated fast class was affected: rejected=%d state=%s", fast.stats.rejected, fast.levee.State())
	}
	if shared.stats.rejectedByClass["fast"] == 0 {
		t.Fatalf("shared controller never rejected healthy fast traffic; shared stats=%+v", shared.stats)
	}
	if shared.stats.transitions[levee.TriggerFailureRate] == 0 {
		t.Fatalf("shared controller never observed statistical failure evidence: %+v", shared.stats.transitions)
	}
	if slow.stats.transitions[levee.TriggerConsecutiveFailures]+slow.stats.transitions[levee.TriggerFailureRate] == 0 {
		t.Fatalf("isolated failing class was not contained: %+v", slow.stats.transitions)
	}
	t.Logf("mixed classes: shared Levee rejected %d healthy fast calls; split fast Levee rejected %d; isolated slow triggers=%+v", shared.stats.rejectedByClass["fast"], fast.stats.rejected, slow.stats.transitions)
}

func TestCorrelatedFailuresAndRetriesAccelerateTrip(t *testing.T) {
	slo := levee.SLO{SuccessRate: 0.95, Timeout: time.Second}
	start := time.Unix(0, 0)

	warmed := func() (*levee.Levee, time.Time) {
		l := levee.NewLevee(slo)
		ts := warmHealthy(t, l, start, 200, 10*time.Millisecond, time.Millisecond)
		return l, ts
	}
	drive := func(l *levee.Levee, ts time.Time, outcomes func(int) bool) (int, levee.Trigger) {
		for attempt := 1; attempt <= 20_000; attempt++ {
			success := outcomes(attempt)
			sc, err := l.Start(ts)
			if err != nil {
				// Admission rejection after a transition is not another backend attempt.
				ts = ts.Add(10 * time.Millisecond)
				continue
			}
			if success {
				sc = l.Success(ts.Add(time.Millisecond), time.Millisecond)
			} else {
				sc = l.Fail(ts.Add(time.Millisecond), time.Millisecond)
			}
			if sc.Trigger != levee.TriggerNone {
				return attempt, sc.Trigger
			}
			ts = ts.Add(10 * time.Millisecond)
		}
		t.Fatal("workload did not transition")
		return 0, levee.TriggerNone
	}

	dispersed, ts := warmed()
	dispersedAttempts, dispersedTrigger := drive(dispersed, ts, func(attempt int) bool {
		return attempt%5 != 1 // 20% failures, never consecutive
	})
	correlated, ts := warmed()
	correlatedAttempts, correlatedTrigger := drive(correlated, ts, func(attempt int) bool {
		return attempt > 7 // one correlated run at the same overall evidence frontier
	})
	retried, ts := warmed()
	retryAttempts, retryTrigger := drive(retried, ts, func(attempt int) bool {
		return false // failed logical calls plus immediate failed retries
	})

	if dispersedTrigger != levee.TriggerFailureRate {
		t.Fatalf("dispersed trigger = %s, want FAILURE_RATE", dispersedTrigger)
	}
	if correlatedTrigger != levee.TriggerConsecutiveFailures || retryTrigger != levee.TriggerConsecutiveFailures {
		t.Fatalf("correlated/retry triggers = %s/%s", correlatedTrigger, retryTrigger)
	}
	if correlatedAttempts >= dispersedAttempts || retryAttempts >= dispersedAttempts {
		t.Fatalf("attempts to trip: dispersed=%d correlated=%d retries=%d", dispersedAttempts, correlatedAttempts, retryAttempts)
	}
	t.Logf("20%% dispersed failures needed %d attempts; a correlated run needed %d; retry-amplified consecutive failures needed %d physical attempts (~%d logical calls at two retries each)", dispersedAttempts, correlatedAttempts, retryAttempts, (retryAttempts+2)/3)
}
