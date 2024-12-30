package levee

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type CircuitBreaker struct {
	mu          sync.Mutex
	stated_slo  SLO
	revised_slo SLO
	metrics     metrics
	concurrents int32
	state       State
	lastOpenAt  time.Time
}

var (
	ErrCircuitOpen     = errors.New("circuit is open")
	ErrCircuitHalfOpen = errors.New("circuit is half open")
)

func NewCircuitBreaker(slo SLO, size uint16) *CircuitBreaker {
	return &CircuitBreaker{
		stated_slo:  slo,
		revised_slo: slo,
		metrics:     *newMetrics(size),
		state:       CLOSED,
	}
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
	start := time.Now()
	state := cb.State()

	if state == OPEN {
		cb.mu.Lock()
		lastOpenAt := cb.lastOpenAt
		timeout := cb.revised_slo.Timeout
		cb.mu.Unlock()

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

	call_err := f()

	end := time.Now()

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
}

func (cb *CircuitBreaker) allowCall() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// historicals
	hErrors := cb.metrics.errors.MeanMid()
	hConcurrency := cb.metrics.concurrency.MeanMid()

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
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state != HALF_OPEN {
		return cb.state
	}

	if cb.metrics.errors.Mean() > (1 - cb.revised_slo.SuccessRate) {
		return OPEN
	}

	hErrors := cb.metrics.errors.MeanMid()
	if hErrors == 0 {
		hErrors = 0.1
	}
	if cb.metrics.errors.FillRate()*float64(cb.metrics.errors._size) > 1/hErrors {
		return CLOSED
	}

	return HALF_OPEN
}

func (cb *CircuitBreaker) mustOpen() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	health := 0

	var success_rate float64
	var latency_dev float64
	var concurrency_dev float64
	var rps float64

	// If the circuit is in the half-open state, use the revised SLO
	success_rate = 1 - cb.metrics.errors.Mean()
	latency_dev = cb.metrics.latency.Deviation()
	concurrency_dev = cb.metrics.concurrency.Deviation()
	rps = cb.metrics.requests.Derivative()

	// Success Rate
	if success_rate < cb.revised_slo.SuccessRate {
		health += 3
	}

	// If there is increased load on the system, at most two of the following
	// metrics can spike while the other remains normal in a healthy system.
	// If all three metrics spike, the system is unhealthy.

	// Latency Anomaly
	if latency_dev > 10*cb.metrics.latency.DeviationMid() || latency_dev > 5*cb.metrics.latency.DeviationLong() {
		health += 1
	}

	// Concurrency Anomaly
	if concurrency_dev > 10*cb.metrics.concurrency.DeviationMid() || concurrency_dev > 5*cb.metrics.concurrency.DeviationLong() {
		health += 1
	}

	// RPS Anomaly
	if rps > 10*cb.metrics.requests.DerivativeMid() || rps > 5*cb.metrics.requests.DerivativeLong() {
		health += 1
	}

	return health >= 3
}

func (cb *CircuitBreaker) OpenCircuit() (State, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.metrics.Reset()
	cb.state = OPEN
	cb.lastOpenAt = time.Now()
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
	cb.mu.Lock()
	defer cb.mu.Unlock()

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
