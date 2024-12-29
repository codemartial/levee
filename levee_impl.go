package levee

import (
	"errors"
	"time"
)

type CircuitBreaker struct {
	stated_slo  SLO
	revised_slo SLO
	metrics     metrics
	concurrents uint32
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

func (cb *CircuitBreaker) Call(f func() error) (State, error) {
	start := time.Now()
	if cb.state == OPEN {
		if time.Since(cb.lastOpenAt) < cb.revised_slo.Timeout {
			return cb.state, ErrCircuitOpen
		} else {
			cb.state = HALF_OPEN
		}
	}

	cb.concurrents++
	defer func() { cb.concurrents-- }()

	if cb.state == HALF_OPEN {
		if !cb.allowCall() {
			return cb.state, ErrCircuitHalfOpen
		}
	}

	if cb.state == CLOSED && cb.mustOpen() {
		return cb.OpenCircuit()
	}

	cb.metrics.RecordConcurrency(float64(cb.concurrents), start)
	cb.metrics.RecordRequests(1, start)

	call_err := f()

	end := time.Now()
	cb.metrics.RecordLatency(float64(end.Sub(start).Microseconds()), end)

	if call_err != nil {
		cb.metrics.RecordErrors(1, end)
	} else {
		cb.metrics.RecordErrors(0, end)
	}

	if cb.state == HALF_OPEN {
		switch cb.newState() {
		case OPEN:
			return cb.OpenCircuit()
		case CLOSED:
			return cb.CloseCircuit()
		default:
			return cb.state, call_err
		}
	}

	return cb.state, nil
}

func (cb *CircuitBreaker) allowCall() bool {
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
	cb.metrics.Reset()
	cb.state = OPEN
	cb.lastOpenAt = time.Now()
	return cb.state, nil
}

func (cb *CircuitBreaker) CloseCircuit() (State, error) {
	cb.metrics.Reset()
	cb.state = CLOSED
	return cb.state, nil
}

func (cb *CircuitBreaker) State() State {
	return cb.state
}

func (cb *CircuitBreaker) StateUpdates() <-chan State {
	return nil
}

type WarmupCB struct {
	CircuitBreaker
	reqCount uint32
	start    time.Time
	end      time.Time
}

func NewWarmupCB(slo SLO) *WarmupCB {
	cb := NewCircuitBreaker(slo, 100)
	cb.state = INIT
	return &WarmupCB{
		CircuitBreaker: *cb,
	}
}

func (cb *WarmupCB) Call(f func() error) (State, error) {
	now := time.Now()
	state, err := cb.CircuitBreaker.Call(f)

	if now.Sub(cb.start) > cb.stated_slo.Warmup {
		cb.reqCount++
	}
	if cb.reqCount > 1000 {
		cb.end = now
		state = CLOSED
	}

	return state, err
}
