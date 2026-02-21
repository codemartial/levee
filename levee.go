package levee

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type SLO struct {
	SuccessRate float64
	Timeout     time.Duration
}

// State represents the circuit breaker state
type State uint8

const (
	CLOSED    State = iota
	OPEN            // Initial cooldown, then rate-limited probing until recovery
	THROTTLED       // Rate-limiting state based on latency anomaly detection
)

type Trigger error

type StateChange struct {
	State   State
	Trigger Trigger
}

var (
	ErrCircuitOpen      = errors.New("circuit is open")
	ErrCircuitThrottled = errors.New("circuit is throttled")
)

// Trigger constants for state changes
var (
	TriggerNone                 Trigger = triggerError("no state change")
	TriggerSLOViolation         Trigger = triggerError("SLO violation")
	TriggerLatencyAnomaly       Trigger = triggerError("latency anomaly")
	TriggerRecoverySucceeded    Trigger = triggerError("recovery succeeded")
	TriggerRecoveryFailed       Trigger = triggerError("recovery failed")
	TriggerTimeoutExpired       Trigger = triggerError("timeout expired")
	TriggerThrottlingStabilised Trigger = triggerError("throttling stabilised")
)

type triggerError string

func (e triggerError) Error() string { return string(e) }

// Levee is an adaptive circuit breaker
type Levee struct {
	mu          sync.RWMutex
	slo         SLO
	metrics     metrics
	concurrents int32
	state       State
	lastOpenAt  atomic.Value

	// OPEN state phases: cooldown (wait for timeout) then probing
	cooldownComplete bool

	// THROTTLED state: AIMD-based concurrency control
	throttleConcurrency   atomic.Uint64 // prevailing concurrency in THROTTLED state
	throttleTargetLatency float64       // baseline latency when throttling started
	throttleSampleCount   int           // samples since last AIMD adjustment
}

func (l *Levee) loadThrottleConcurrency() float64 {
	return math.Float64frombits(l.throttleConcurrency.Load())
}

func (l *Levee) storeThrottleConcurrency(v float64) {
	l.throttleConcurrency.Store(math.Float64bits(v))
}

func NewLevee(slo SLO) *Levee {
	l := &Levee{
		slo:     slo,
		metrics: *newMetrics(100),
		state:   CLOSED,
	}
	l.lastOpenAt.Store(time.Time{})
	return l
}

func (l *Levee) AddConcurrent() {
	atomic.AddInt32(&l.concurrents, 1)
}

func (l *Levee) RemoveConcurrent() {
	atomic.AddInt32(&l.concurrents, -1)
}

func (l *Levee) Concurrents() int32 {
	return atomic.LoadInt32(&l.concurrents)
}

func (l *Levee) Start(ts time.Time) (StateChange, error) {
	state := l.State()
	trigger := TriggerNone

	// OPEN state handling (with cooldown)
	if state == OPEN {
		lastOpenAt := l.lastOpenAt.Load().(time.Time)
		timeout := l.slo.Timeout

		l.mu.Lock()
		// Check cooldown phase
		if !l.cooldownComplete {
			if ts.Sub(lastOpenAt) < timeout {
				l.mu.Unlock()
				return StateChange{State: OPEN}, ErrCircuitOpen
			}
			// Cooldown complete, enter probing phase
			l.cooldownComplete = true
			trigger = TriggerTimeoutExpired
		}
		l.mu.Unlock()

		// Probing phase: rate-limited calls via probingAllowed()
		l.AddConcurrent()
		if !l.probingAllowed() {
			l.RemoveConcurrent()
			return StateChange{State: OPEN, Trigger: trigger}, ErrCircuitOpen
		}

		return StateChange{State: OPEN, Trigger: trigger}, nil
	}

	// CLOSED/THROTTLED state handling (both are normal operational states)
	l.AddConcurrent()

	// Check if we should open circuit (SLO violation only)
	shouldOpen, openTrigger := l.mustOpen()
	if shouldOpen {
		l.RemoveConcurrent()
		l.OpenCircuit(ts)
		return StateChange{State: OPEN, Trigger: openTrigger}, ErrCircuitOpen
	}

	// Check for latency anomaly to decide throttling
	hasAnomaly, anomalyConcurrency := l.hasLatencyAnomaly()

	// Handle CLOSED state
	if state == CLOSED {
		if !hasAnomaly {
			// CLOSED → CLOSED: normal operation
			return StateChange{State: CLOSED, Trigger: TriggerNone}, nil
		}

		// CLOSED → THROTTLED: latency anomaly detected
		l.mu.RLock()
		floor := l.metrics.latency.Stat(TMean, Long) / 1_000_000
		targetLatency := l.metrics.latency.Mean()
		l.mu.RUnlock()

		ceiling := max(anomalyConcurrency, floor)
		l.EnterThrottled(ceiling, targetLatency)

		// Re-read actual ceiling (another goroutine may have set a different value)
		ceiling = l.loadThrottleConcurrency()
		if ceiling == 0 {
			// Throttling was exited by another goroutine, proceed as CLOSED
			return StateChange{State: CLOSED, Trigger: TriggerNone}, nil
		}

		if float64(l.Concurrents()) > ceiling {
			l.RemoveConcurrent()
			return StateChange{State: THROTTLED, Trigger: TriggerLatencyAnomaly}, ErrCircuitThrottled
		}

		return StateChange{State: THROTTLED, Trigger: TriggerLatencyAnomaly}, nil
	}

	// Handle THROTTLED state
	if !hasAnomaly && l.throttlingStabilised() {
		// THROTTLED → CLOSED: errors stabilised
		l.mu.Lock()
		l.state = CLOSED
		l.storeThrottleConcurrency(0)
		l.mu.Unlock()
		return StateChange{State: CLOSED, Trigger: TriggerThrottlingStabilised}, nil
	}

	// THROTTLED → THROTTLED: continue with AIMD adjustment
	l.mu.Lock()
	l.throttleSampleCount++
	if l.throttleSampleCount >= 50 {
		l.throttleSampleCount = 0
		currentLatency := l.metrics.latency.Mean()
		floor := l.metrics.latency.Stat(TMean, Long) / 1_000_000
		currentCeiling := l.loadThrottleConcurrency()

		if currentLatency <= l.throttleTargetLatency*1.1 {
			// Latency stable - increase ceiling
			currentCeiling *= 1.1
		} else if hasAnomaly {
			// Latency elevated - decrease ceiling aggressively
			currentCeiling *= 0.5
			l.throttleTargetLatency = currentLatency
		}

		// Floor at long-term average
		if currentCeiling < floor {
			currentCeiling = floor
		}
		l.storeThrottleConcurrency(currentCeiling)
	}
	ceiling := l.loadThrottleConcurrency()
	l.mu.Unlock()

	if float64(l.Concurrents()) > ceiling {
		l.RemoveConcurrent()
		return StateChange{State: THROTTLED, Trigger: TriggerNone}, ErrCircuitThrottled
	}

	return StateChange{State: THROTTLED, Trigger: TriggerNone}, nil
}

func (l *Levee) Success(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, true)
}

func (l *Levee) Fail(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, false)
}

func (l *Levee) processResult(ts time.Time, duration time.Duration, success bool) StateChange {
	defer l.RemoveConcurrent()

	successCount := 1.0
	if !success {
		successCount = 0.0
	}
	l.mu.Lock()
	l.metrics.RecordLatency(float64(duration.Microseconds()), ts)
	l.metrics.RecordSuccesses(successCount, ts)

	state := l.state
	inProbingPhase := state == OPEN && l.cooldownComplete
	l.mu.Unlock()

	// Check recovery during OPEN probing phase
	if inProbingPhase {
		newState, trigger := l.newState()
		switch newState {
		case OPEN:
			if trigger == TriggerRecoveryFailed {
				// Recovery failed, reset cooldown to wait again
				l.resetCooldown(ts)
			}
			// Otherwise continue probing (TriggerNone = not enough confidence yet)
			return StateChange{State: OPEN, Trigger: trigger}
		case CLOSED:
			l.CloseCircuit()
			return StateChange{State: CLOSED, Trigger: trigger}
		}
	}

	return StateChange{State: state, Trigger: TriggerNone}
}

func (l *Levee) Call(f func() error) (StateChange, error) {
	start := time.Now()

	sc, err := l.Start(start)
	if err != nil {
		return sc, err
	}

	call_err := f()
	end := time.Now()
	duration := end.Sub(start)

	if call_err != nil {
		return l.Fail(end, duration), call_err
	}
	return l.Success(end, duration), call_err
}

func (l *Levee) OpenCircuit(ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state == OPEN && !l.cooldownComplete {
		return // Already in cooldown
	}
	l.metrics.Reset()
	l.state = OPEN
	l.cooldownComplete = false
	l.lastOpenAt.Store(ts)
}

// resetCooldown resets the cooldown timer after a failed recovery attempt
func (l *Levee) resetCooldown(ts time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cooldownComplete = false
	l.lastOpenAt.Store(ts)
	l.metrics.Reset()
}

func (l *Levee) CloseCircuit() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state == CLOSED {
		return
	}
	l.state = CLOSED
	l.cooldownComplete = false
	l.storeThrottleConcurrency(0)
}

// EnterThrottled transitions to THROTTLED state with the given concurrency limit
func (l *Levee) EnterThrottled(safeConcurrency, targetLatency float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state == THROTTLED {
		return
	}

	l.storeThrottleConcurrency(safeConcurrency)
	l.throttleTargetLatency = targetLatency
	l.throttleSampleCount = 0
	l.state = THROTTLED
	// Note: Do NOT reset metrics here - we need to preserve history for mustOpen() checks
}

func (l *Levee) State() State {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.state
}

func (l *Levee) StateUpdates() <-chan StateChange {
	return nil
}

// Expunge resets the circuit breaker to CLOSED state with fresh metrics
func (l *Levee) Expunge() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.metrics = *newMetrics(100)
	l.state = CLOSED
	l.cooldownComplete = false
	l.lastOpenAt.Store(time.Time{})
}
