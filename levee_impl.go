package levee

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type CircuitBreaker struct {
	mu          sync.RWMutex
	stated_slo  SLO
	revised_slo SLO
	metrics     metrics
	concurrents int32
	state       State
	lastOpenAt  atomic.Value
}

var (
	ErrCircuitOpen     = errors.New("circuit is open")
	ErrCircuitHalfOpen = errors.New("circuit is half open")
)

// Trigger constants for state changes
var (
	TriggerNone              Trigger = triggerError("no state change")
	TriggerSLOViolation      Trigger = triggerError("SLO violation")
	TriggerLatencyAnomaly    Trigger = triggerError("latency anomaly")
	TriggerRecoverySucceeded Trigger = triggerError("recovery succeeded")
	TriggerRecoveryFailed    Trigger = triggerError("recovery failed")
	TriggerTimeoutExpired    Trigger = triggerError("timeout expired")
	TriggerWarmupComplete    Trigger = triggerError("warmup complete")
)

type triggerError string

func (e triggerError) Error() string { return string(e) }

func NewCircuitBreaker(slo SLO, size uint16) *CircuitBreaker {
	cb := &CircuitBreaker{
		stated_slo:  slo,
		revised_slo: slo,
		metrics:     *newMetrics(size),
		state:       CLOSED,
	}
	cb.lastOpenAt.Store(time.Time{})
	return cb
}

func (cb *CircuitBreaker) AddConcurrent() {
	atomic.AddInt32(&cb.concurrents, 1)
}

func (cb *CircuitBreaker) RemoveConcurrent() {
	atomic.AddInt32(&cb.concurrents, -1)
}

func (cb *CircuitBreaker) Concurrents() int32 {
	return atomic.LoadInt32(&cb.concurrents)
}

func (cb *CircuitBreaker) Start(ts time.Time) (StateChange, error) {
	// START PRE-CALL CHECKS
	state := cb.State()
	trigger := TriggerNone

	if state == OPEN {
		lastOpenAt := cb.lastOpenAt.Load().(time.Time)
		timeout := cb.revised_slo.Timeout

		if ts.Sub(lastOpenAt) < timeout {
			return StateChange{State: state, Trigger: TriggerNone}, ErrCircuitOpen
		}
		cb.mu.Lock()
		cb.state = HALF_OPEN
		state = cb.state
		trigger = TriggerTimeoutExpired
		cb.mu.Unlock()
		// State changed OPEN -> HALF_OPEN, continue processing
	}

	cb.AddConcurrent()

	if state == HALF_OPEN && !cb.allowCall() {
		cb.RemoveConcurrent()
		return StateChange{State: cb.State(), Trigger: trigger}, ErrCircuitHalfOpen
	}

	if state == CLOSED {
		shouldOpen, openTrigger := cb.mustOpen()
		if shouldOpen {
			cb.RemoveConcurrent()
			cb.OpenCircuit(ts)
			return StateChange{State: OPEN, Trigger: openTrigger}, ErrCircuitOpen
		}
	}

	cb.mu.Lock()
	cb.metrics.RecordConcurrency(float64(cb.Concurrents()))
	cb.mu.Unlock()
	// START CALL
	return StateChange{State: state, Trigger: trigger}, nil
}

func (cb *CircuitBreaker) Success(ts time.Time, duration time.Duration) StateChange {
	return cb.processResult(ts, duration, true)
}

func (cb *CircuitBreaker) Fail(ts time.Time, duration time.Duration) StateChange {
	return cb.processResult(ts, duration, false)
}

func (cb *CircuitBreaker) processResult(ts time.Time, duration time.Duration, success bool) StateChange {
	defer cb.RemoveConcurrent()
	// END POST-CALL PROCESSING

	errCount := 0.0
	if !success {
		errCount = 1.0
	}
	cb.mu.Lock()
	cb.metrics.RecordLatency(float64(duration.Microseconds()))
	cb.metrics.RecordErrors(errCount)
	cb.mu.Unlock()

	state := cb.State()
	if state == HALF_OPEN {
		newState, trigger := cb.newState()
		switch newState {
		case OPEN:
			cb.OpenCircuit(ts)
			return StateChange{State: OPEN, Trigger: trigger}
		case CLOSED:
			cb.CloseCircuit()
			return StateChange{State: CLOSED, Trigger: trigger}
		default:
			return StateChange{State: state, Trigger: trigger}
		}
	}

	return StateChange{State: state, Trigger: TriggerNone}
}

func (cb *CircuitBreaker) Call(f func() error) (StateChange, error) {
	start := time.Now()

	// Use Start() to perform pre-call checks
	sc, err := cb.Start(start)
	if err != nil {
		return sc, err
	}

	// Execute the function
	call_err := f()
	end := time.Now()
	duration := end.Sub(start)

	// Process the result
	if call_err != nil {
		return cb.Fail(end, duration), call_err
	}
	return cb.Success(end, duration), call_err
}

func (cb *CircuitBreaker) allowCall() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	if cb.State() != HALF_OPEN {
		panic("Bug Encountered. This method must only be called when breaker is half open")
	}

	// historicals
	hErrors := cb.metrics.errors.Stat(Mean, Mid)
	hConcurrency := cb.metrics.concurrency.Stat(Mean, Mid)

	var allowedConcurrency int32 = 1
	if hConcurrency > 0 {
		allowedConcurrency = max(1, int32((1-hErrors)*hConcurrency))
	}

	return cb.concurrents <= allowedConcurrency
}

func (cb *CircuitBreaker) newState() (State, Trigger) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if cb.state != HALF_OPEN {
		return cb.state, TriggerNone
	}

	const minSamples = 10
	n := float64(cb.metrics.errors.RawValueCount())

	if n < minSamples {
		return HALF_OPEN, TriggerNone
	}

	rawErrorRate := cb.metrics.errors.Mean()
	requiredSuccessRate := cb.revised_slo.SuccessRate

	// Use Adjusted Wald method to compute confidence interval for success rate
	// z=1.96 for 95% confidence
	const z = 1.96
	const z2 = z * z

	successCount := n * (1 - rawErrorRate)
	nAdj := n + z2
	pTilde := (successCount + z2/2) / nAdj
	se := math.Sqrt(pTilde * (1 - pTilde) / nAdj)

	lowerBound := pTilde - z*se // Lower bound of success rate confidence interval
	upperBound := pTilde + z*se // Upper bound of success rate confidence interval

	if upperBound < requiredSuccessRate {
		return OPEN, TriggerRecoveryFailed
	}

	if lowerBound >= requiredSuccessRate {
		return CLOSED, TriggerRecoverySucceeded
	}

	// Not enough confidence yet, keep testing
	return HALF_OPEN, TriggerNone
}

func (cb *CircuitBreaker) mustOpen() (bool, Trigger) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if !cb.metrics.hasSufficientHistory() {
		return false, TriggerNone
	}

	n := float64(cb.metrics.errors.RawValueCount())
	errorMean := cb.metrics.errors.Mean()
	rawSuccessRate := 1 - errorMean

	// Adjusted Wald method for confidence interval
	const z = 1.96 // 95% confidence
	const z2 = z * z

	successCount := n * rawSuccessRate
	nAdj := n + z2
	pTilde := (successCount + z2/2) / nAdj
	se := math.Sqrt(pTilde * (1 - pTilde) / nAdj)

	upperBound := pTilde + z*se // Upper bound of success rate confidence interval

	if upperBound < cb.stated_slo.SuccessRate {
		return true, TriggerSLOViolation
	}

	// Trend check: compare live Mean() to Base (last buffer wrap)
	// If both latency and concurrency are trending down, situation is improving
	currentLatency := cb.metrics.latency.Mean()
	baseLatency := cb.metrics.latency.Stat(Mean, Base)
	currentConcurrency := cb.metrics.concurrency.Mean()
	baseConcurrency := cb.metrics.concurrency.Stat(Mean, Base)

	if currentLatency < baseLatency && currentConcurrency < baseConcurrency {
		return false, TriggerNone
	}

	// Check against both time horizons
	midAnomaly := unexpectedLatencySpike(&cb.metrics, Mid)
	longAnomaly := unexpectedLatencySpike(&cb.metrics, Long)

	if midAnomaly && longAnomaly {
		return true, TriggerLatencyAnomaly
	}
	return false, TriggerNone
}

func unexpectedLatencySpike(m *metrics, horizon StatRange) bool {
	const epsilon = 1e-9

	currentLatency := m.latency.Mean()
	currentConcurrency := m.concurrency.Mean()
	historicalLatency := m.latency.Stat(Mean, horizon)
	historicalConcurrency := m.concurrency.Stat(Mean, horizon)
	historicalLatencyDev := m.latency.Stat(Deviation, horizon)

	// RPS calculation
	currentRPS := currentConcurrency / max(currentLatency, epsilon)
	historicalRPS := historicalConcurrency / max(historicalLatency, epsilon)

	// RPS multiplier: how much did traffic change?
	rpsX := currentRPS / max(historicalRPS, epsilon)

	// Expected latency multiplier: sub-linear scaling with load
	var expectedLatencyX float64
	if rpsX >= 1.0 {
		expectedLatencyX = 1.0 + math.Log(rpsX)
	} else {
		expectedLatencyX = 1.0 // No increase expected when load drops
	}

	// Actual latency multiplier
	actualLatencyX := currentLatency / max(historicalLatency, epsilon)

	// CV-based tolerance for natural variance
	cvLatency := historicalLatencyDev / max(historicalLatency, epsilon)
	tolerance := 1.0 + 3.0*cvLatency

	// Threshold: expected increase with variance tolerance
	threshold := expectedLatencyX * tolerance

	return actualLatencyX > threshold
}

func (cb *CircuitBreaker) OpenCircuit(ts time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == OPEN {
		return
	}
	cb.metrics.Reset()
	cb.state = OPEN
	cb.lastOpenAt.Store(ts)
}

func (cb *CircuitBreaker) CloseCircuit() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == CLOSED {
		return
	}
	cb.metrics.Reset()
	cb.state = CLOSED
}

func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	return cb.state
}

func (cb *CircuitBreaker) StateUpdates() <-chan StateChange {
	return nil
}

type WarmupCB struct {
	mu           sync.RWMutex
	slo          SLO
	state        State
	start        time.Time
	end          time.Time
	lastOpenAt   time.Time
	successCount uint32
	failureCount uint32
	reqCount     uint32
}

func NewWarmupCB(slo SLO) *WarmupCB {
	return &WarmupCB{
		slo:   slo,
		state: INIT,
	}
}

func (cb *WarmupCB) Start(ts time.Time) (StateChange, error) {
	cb.mu.Lock()

	if cb.start.IsZero() {
		cb.start = ts
	}

	trigger := TriggerNone
	if cb.state == OPEN {
		if ts.Sub(cb.lastOpenAt) < cb.slo.Timeout {
			cb.mu.Unlock()
			return StateChange{State: OPEN, Trigger: TriggerNone}, ErrCircuitOpen
		}
		// Timeout expired, transition to HALF_OPEN
		cb.state = HALF_OPEN
		trigger = TriggerTimeoutExpired
	}

	cb.mu.Unlock()
	return StateChange{State: cb.state, Trigger: trigger}, nil
}

func (cb *WarmupCB) Success(ts time.Time, duration time.Duration) StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.successCount++
	trigger := TriggerNone

	// Only count requests after warmup period in CLOSED state
	if ts.Sub(cb.start) > cb.slo.Warmup && cb.state == CLOSED {
		cb.reqCount++
		if cb.reqCount > 1000 {
			cb.end = ts
		}
	}

	// Check if we should transition from HALF_OPEN to CLOSED
	if cb.state == HALF_OPEN {
		totalCalls := cb.successCount + cb.failureCount
		if totalCalls >= 10 {
			successRate := float64(cb.successCount) / float64(totalCalls)
			if successRate >= cb.slo.SuccessRate {
				cb.state = CLOSED
				cb.successCount = 0
				cb.failureCount = 0
				cb.reqCount = 0 // Reset count when entering CLOSED from HALF_OPEN
				trigger = TriggerRecoverySucceeded
			} else {
				cb.state = OPEN
				cb.lastOpenAt = ts
				cb.successCount = 0
				cb.failureCount = 0
				trigger = TriggerRecoveryFailed
			}
		}
	} else if cb.state == INIT {
		// Transition from INIT to CLOSED on first success after warmup
		if ts.Sub(cb.start) > cb.slo.Warmup {
			cb.state = CLOSED
			cb.successCount = 0
			cb.failureCount = 0
			cb.reqCount = 0 // Reset count when entering CLOSED from INIT
			trigger = TriggerWarmupComplete
		}
	}

	return StateChange{State: cb.state, Trigger: trigger}
}

func (cb *WarmupCB) Fail(ts time.Time, duration time.Duration) StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failureCount++
	trigger := TriggerNone

	// Count requests after warmup period in CLOSED state (failures count too)
	if ts.Sub(cb.start) > cb.slo.Warmup && cb.state == CLOSED {
		cb.reqCount++
		if cb.reqCount > 1000 {
			cb.end = ts
			// Keep state as CLOSED - will be detected by Levee for transition
		}
	}

	// Check if we should open based on simple threshold
	totalCalls := cb.successCount + cb.failureCount
	if totalCalls >= 10 {
		successRate := float64(cb.successCount) / float64(totalCalls)
		if successRate < cb.slo.SuccessRate {
			prevState := cb.state
			cb.state = OPEN
			cb.lastOpenAt = ts
			cb.successCount = 0
			cb.failureCount = 0
			trigger = TriggerSLOViolation
			// Reset reqCount when transitioning OUT of CLOSED
			if prevState == CLOSED {
				cb.reqCount = 0
			}
		}
	}

	return StateChange{State: cb.state, Trigger: trigger}
}

func (cb *WarmupCB) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

func (cb *WarmupCB) StateUpdates() <-chan StateChange {
	return nil
}

func (cb *WarmupCB) Call(f func() error) (StateChange, error) {
	start := time.Now()

	// Perform pre-call checks
	sc, err := cb.Start(start)
	if err != nil {
		return sc, err
	}

	// Execute the function
	call_err := f()
	end := time.Now()
	duration := end.Sub(start)

	// Process the result
	if call_err != nil {
		return cb.Fail(end, duration), call_err
	}
	return cb.Success(end, duration), call_err
}
