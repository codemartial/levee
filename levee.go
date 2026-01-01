package levee

import (
	"errors"
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
	CLOSED State = iota
	OPEN      // Combines old OPEN + HALF_OPEN: initial cooldown, then rate-limited probing
	THROTTLED // Rate-limiting state based on concurrency-error correlation

	// HALF_OPEN is deprecated - kept for backward compatibility with benchmarks
	// In v0.3.0, HALF_OPEN behavior is merged into OPEN with cooldown phases
	HALF_OPEN = OPEN
)

type Trigger error

type StateChange struct {
	State   State
	Trigger Trigger
}

var (
	ErrCircuitOpen      = errors.New("circuit is open")
	ErrCircuitThrottled = errors.New("circuit is throttled")

	// ErrCircuitHalfOpen is deprecated - kept for backward compatibility
	// In v0.3.0, HALF_OPEN is merged into OPEN with rate-limited probing
	ErrCircuitHalfOpen = ErrCircuitOpen
)

// Trigger constants for state changes
var (
	TriggerNone              Trigger = triggerError("no state change")
	TriggerSLOViolation      Trigger = triggerError("SLO violation")
	TriggerLatencyAnomaly    Trigger = triggerError("latency anomaly")
	TriggerRecoverySucceeded Trigger = triggerError("recovery succeeded")
	TriggerRecoveryFailed    Trigger = triggerError("recovery failed")
	TriggerTimeoutExpired    Trigger = triggerError("timeout expired")

	// THROTTLED state triggers
	TriggerConcurrencyOverload  Trigger = triggerError("concurrency overload")
	TriggerThrottlingFailed     Trigger = triggerError("throttling failed")
	TriggerThrottlingStabilised Trigger = triggerError("throttling stabilised")
)

type triggerError string

func (e triggerError) Error() string { return string(e) }

// Levee is an adaptive circuit breaker
type Levee struct {
	mu          sync.RWMutex
	stated_slo  SLO
	revised_slo SLO
	metrics     metrics
	concurrents int32
	state       State
	lastOpenAt  atomic.Value

	// OPEN state phases: cooldown (wait for timeout) then probing
	cooldownComplete bool

	// THROTTLED state: current safe concurrency limit
	throttleConcurrency float64
}

func NewLevee(slo SLO) *Levee {
	l := &Levee{
		stated_slo:  slo,
		revised_slo: slo,
		metrics:     *newMetrics(100), // Fixed initial size
		state:       CLOSED,
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
		timeout := l.revised_slo.Timeout

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

		// Probing phase: rate-limited calls
		l.AddConcurrent()
		if !l.allowCall() {
			l.RemoveConcurrent()
			return StateChange{State: OPEN, Trigger: trigger}, ErrCircuitOpen
		}

		l.mu.Lock()
		l.metrics.RecordConcurrency(float64(l.Concurrents()), ts)
		l.mu.Unlock()

		return StateChange{State: OPEN, Trigger: trigger}, nil
	}

	l.AddConcurrent()

	// THROTTLED state handling
	if state == THROTTLED {
		if l.throttlingFailed() {
			l.RemoveConcurrent()
			l.OpenCircuit(ts)
			return StateChange{State: OPEN, Trigger: TriggerThrottlingFailed}, ErrCircuitOpen
		}

		// Enforce concurrency limit
		l.mu.RLock()
		throttleLimit := l.throttleConcurrency
		l.mu.RUnlock()

		if float64(l.Concurrents()) > throttleLimit {
			l.RemoveConcurrent()
			return StateChange{State: THROTTLED}, ErrCircuitThrottled
		}

		// Re-evaluate safe concurrency
		l.mu.Lock()
		l.throttleConcurrency = l.safeConcurrency()
		l.metrics.RecordConcurrency(float64(l.Concurrents()), ts)
		l.mu.Unlock()

		return StateChange{State: THROTTLED}, nil
	}

	// CLOSED state handling
	// Check throttle BEFORE mustOpen
	if shouldThrottle, safeConcurrency := l.shouldThrottle(); shouldThrottle {
		l.EnterThrottled(safeConcurrency)
		l.mu.Lock()
		l.metrics.RecordConcurrency(float64(l.Concurrents()), ts)
		l.mu.Unlock()
		return StateChange{State: THROTTLED, Trigger: TriggerConcurrencyOverload}, nil
	}

	shouldOpen, openTrigger := l.mustOpen()
	if shouldOpen {
		l.RemoveConcurrent()
		l.OpenCircuit(ts)
		return StateChange{State: OPEN, Trigger: openTrigger}, ErrCircuitOpen
	}

	l.mu.Lock()
	l.metrics.RecordConcurrency(float64(l.Concurrents()), ts)
	l.mu.Unlock()

	return StateChange{State: CLOSED, Trigger: trigger}, nil
}

func (l *Levee) Success(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, true)
}

func (l *Levee) Fail(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, false)
}

func (l *Levee) processResult(ts time.Time, duration time.Duration, success bool) StateChange {
	defer l.RemoveConcurrent()

	errCount := 0.0
	if !success {
		errCount = 1.0
	}
	l.mu.Lock()
	l.metrics.RecordLatency(float64(duration.Microseconds()), ts)
	l.metrics.RecordErrors(errCount, ts)
	l.mu.Unlock()

	l.mu.RLock()
	state := l.state
	inProbingPhase := state == OPEN && l.cooldownComplete
	l.mu.RUnlock()

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

	// Check THROTTLED stabilisation
	if state == THROTTLED {
		if l.throttlingStabilised() {
			l.CloseCircuit()
			return StateChange{State: CLOSED, Trigger: TriggerThrottlingStabilised}
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
	l.throttleConcurrency = 0
}

// EnterThrottled transitions to THROTTLED state with the given concurrency limit
func (l *Levee) EnterThrottled(safeConcurrency float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state == THROTTLED {
		return
	}

	l.throttleConcurrency = safeConcurrency
	l.state = THROTTLED

	// Reset Base for new state (Mid/Long preserved and continue updating)
	l.metrics.Reset()
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
