package levee

import (
	"context"
	"time"
)

type CircuitBreaker struct {
	stated_slo  SLO
	revised_slo SLO
	successRate metrics
	latency     metrics
	concurrency metrics
	state       State
}

func NewCircuitBreaker(slo SLO, size uint16) *CircuitBreaker {
	return &CircuitBreaker{
		stated_slo:  slo,
		revised_slo: slo,
		successRate: *newMetrics(size),
		latency:     *newMetrics(size),
		concurrency: *newMetrics(size),
		state:       CLOSED,
	}
}

func (cb *CircuitBreaker) Call(func() error) (State, error) {
	return cb.state, nil
}

func (cb *CircuitBreaker) CallWithContext(context.Context, func() error) (State, error) {
	return cb.state, nil
}

func (cb *CircuitBreaker) preCall() {
}

func (cb *CircuitBreaker) postCall() {
}

func (cb *CircuitBreaker) doCall(start time.Time, timeout time.Duration, f func() error) (time.Duration, error) {
	return time.Since(start), f()
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
	if cb.reqCount > 100 {
		cb.end = now
		state = CLOSED
	}

	return state, err
}

func (cb *WarmupCB) CallWithContext(ctx context.Context, f func() error) (State, error) {
	cb.reqCount++
	state, err := cb.CircuitBreaker.CallWithContext(ctx, f)
	if cb.reqCount > 100 && time.Since(cb.start) > cb.stated_slo.Warmup {
		state = CLOSED
	}
	return state, err
}
