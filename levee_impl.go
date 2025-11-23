package levee

import (
	"errors"
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

func (cb *CircuitBreaker) Call(f func() error) (State, error) {
	// START PRE-CALL CHECKS
	start := time.Now()
	state := cb.State()

	if state == OPEN {
		lastOpenAt := cb.lastOpenAt.Load().(time.Time)
		timeout := cb.revised_slo.Timeout

		if time.Since(lastOpenAt) < timeout {
			return state, ErrCircuitOpen
		} else {
			cb.mu.Lock()
			cb.state = HALF_OPEN
			state = cb.state
			cb.mu.Unlock()
		}
	}

	cb.AddConcurrent()
	defer cb.RemoveConcurrent()

	if state == HALF_OPEN && !cb.allowCall() {
		return state, ErrCircuitHalfOpen
	}

	if state == CLOSED && cb.mustOpen() {
		return cb.OpenCircuit()
	}

	{
		cb.mu.Lock()
		cb.metrics.RecordConcurrency(float64(cb.Concurrents()), start)
		cb.metrics.RecordRequests(1, start)
		cb.mu.Unlock()
	}
	// END PRE-CALL CHECKS

	// START CALL
	call_err := f()
	end := time.Now()
	// END Call

	// START POST-CALL PROCESSING
	{
		cb.mu.Lock()
		cb.metrics.RecordLatency(float64(end.Sub(start).Microseconds()), end)

		if call_err != nil {
			cb.metrics.RecordErrors(1, end)
		} else {
			cb.metrics.RecordErrors(0, end)
		}
		cb.mu.Unlock()
	}

	if state == HALF_OPEN {
		switch cb.newState() {
		case OPEN:
			return cb.OpenCircuit()
		case CLOSED:
			return cb.CloseCircuit()
		default:
			return state, call_err
		}
	}

	return state, nil
	// END POST-CALL PROCESSING
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

	var allowedConcurrency float64
	if hErrors == 0 || hConcurrency == 0 {
		allowedConcurrency = 1.0
	} else {
		allowedConcurrency = (1 - hErrors) * hConcurrency
	}

	if float64(cb.concurrents) > allowedConcurrency {
		return false
	}

	return true
}

func (cb *CircuitBreaker) newState() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	if cb.state != HALF_OPEN {
		return cb.state
	}

	if cb.metrics.errors.Mean() > (1 - cb.revised_slo.SuccessRate) {
		return OPEN
	}

	// Check min. consecutive successes based on error rate
	hErrors := max(0.01, cb.metrics.errors.Stat(Mean, Mid))
	if float64(cb.metrics.errors.RawValueCount()) > 1/hErrors {
		return CLOSED
	}

	return HALF_OPEN
}

func (cb *CircuitBreaker) mustOpen() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	faults := 0

	var success_rate float64
	var latency_dev float64
	var concurrency_dev float64
	var rps float64

	// If the circuit is in the half-open state, use the revised SLO
	success_rate = 1 - cb.metrics.errors.Mean()
	latency_dev = cb.metrics.latency.Stat(Deviation, Raw)
	concurrency_dev = cb.metrics.concurrency.Stat(Deviation, Raw)
	rps = cb.metrics.requests.Stat(Derivative, Raw)

	// Success Rate
	if success_rate < cb.revised_slo.SuccessRate {
		faults += 3
	}

	// If there is increased load on the system, at most two of the following
	// metrics can spike while the other remains normal in a healthy system.
	// If all three metrics spike, the system is unhealthy.

	// Latency Anomaly
	if latency_dev > 10*cb.metrics.latency.Stat(Deviation, Mid) || latency_dev > 5*cb.metrics.latency.Stat(Deviation, Long) {
		faults += 1
	}

	// Concurrency Anomaly
	if concurrency_dev > 10*cb.metrics.concurrency.Stat(Deviation, Mid) ||
		concurrency_dev > 5*cb.metrics.concurrency.Stat(Deviation, Long) {
		faults += 1
	}

	// RPS Anomaly
	if rps > 10*cb.metrics.requests.Stat(Derivative, Mid) || rps > 5*cb.metrics.requests.Stat(Derivative, Long) {
		faults += 1
	}

	return faults >= 3
}

func (cb *CircuitBreaker) OpenCircuit() (State, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.metrics.Reset()
	cb.state = OPEN
	cb.lastOpenAt.Store(time.Now())
	return cb.state, nil
}

func (cb *CircuitBreaker) CloseCircuit() (State, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.metrics.Reset()
	cb.state = CLOSED
	return cb.state, nil
}

func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	return cb.state
}

func (cb *CircuitBreaker) StateUpdates() <-chan State {
	return nil
}

type WarmupCB struct {
	*CircuitBreaker
	reqCount uint32
	start    time.Time
	end      time.Time
}

func NewWarmupCB(slo SLO) *WarmupCB {
	cb := NewCircuitBreaker(slo, 100)
	cb.state = INIT
	return &WarmupCB{
		CircuitBreaker: cb,
		start:          time.Now(),
	}
}

func (cb *WarmupCB) Call(f func() error) (State, error) {
	now := time.Now()

	// The following call uses its own locking
	_, err := cb.CircuitBreaker.Call(f)

	cb.mu.Lock()
	defer cb.mu.Unlock()
	if now.Sub(cb.start) > cb.stated_slo.Warmup {
		cb.reqCount++
	}
	if cb.reqCount > 1000 {
		cb.end = now
		cb.state = CLOSED
	}

	return cb.state, err
}
