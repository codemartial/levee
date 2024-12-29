package levee

import (
	"context"
	"time"
)

type SLO struct {
	SuccessRate float64
	Timeout     time.Duration
	Warmup      time.Duration
}

// enum for circuit breaker states
type State uint8

const (
	INIT State = iota
	CLOSED
	OPEN
	HALF_OPEN
)

type ICircuitBreaker interface {
	// Call executes the given function and returns the state of the circuit breaker and any error
	Call(func() error) (State, error)
	CallWithContext(context.Context, func() error) (State, error)
	State() State
	StateUpdates() <-chan State
}

type Levee struct {
	ready bool
	wu    *WarmupCB
	cb    *CircuitBreaker
	state chan State
}

func NewLevee(slo SLO) *Levee {
	return &Levee{
		ready: false,
		wu:    NewWarmupCB(slo),
	}
}

func (l *Levee) Call(f func() error) (State, error) {
	if !l.ready {
		s, err := l.wu.Call(f)
		if s == CLOSED {
			rps := float64(l.wu.reqCount) / (l.wu.end.Sub(l.wu.start).Seconds() - l.wu.stated_slo.Warmup.Seconds())
			rps = max(rps, 100)
			rps = min(rps, 1<<16-1)
			l.cb = NewCircuitBreaker(l.wu.stated_slo, uint16(rps))
			l.cb.state = CLOSED
			l.ready = true
		}
		return s, err
	}
	return l.cb.Call(f)
}

func (l *Levee) CallWithContext(ctx context.Context, f func() error) (State, error) {
	if !l.ready {
		s, err := l.wu.Call(f)
		if s == CLOSED {
			size := float64(l.wu.reqCount) / l.wu.stated_slo.Warmup.Seconds()
			size = max(size, 100)
			size = min(size, 1<<16-1)
			l.cb = NewCircuitBreaker(l.wu.stated_slo, uint16(size))
			l.cb.state = CLOSED
			l.ready = true
		}
		return s, err
	}
	return l.cb.CallWithContext(ctx, f)
}

func (l *Levee) State() State {
	return l.cb.State()
}

func (l *Levee) StateUpdates() <-chan State {
	return l.cb.StateUpdates()
}
