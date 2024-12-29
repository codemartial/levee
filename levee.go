package levee

import (
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
	State() State
	StateUpdates() <-chan State
}

type Levee struct {
	ready bool
	cb    ICircuitBreaker
	state chan State
}

func NewLevee(slo SLO) *Levee {
	return &Levee{
		ready: false,
		cb:    NewWarmupCB(slo),
		state: make(chan State),
	}
}

func (l *Levee) Call(f func() error) (State, error) {
	if !l.ready {
		wu := l.cb.(*WarmupCB)
		s, err := wu.Call(f)
		if s == CLOSED {
			rps := float64(wu.reqCount) / (wu.end.Sub(wu.start).Seconds() - wu.stated_slo.Warmup.Seconds())
			sloBasedSamples := 10 / (1 - wu.stated_slo.SuccessRate)
			rps = max(rps, 100, sloBasedSamples)
			rps = min(rps, 1<<16-1)
			l.cb = NewCircuitBreaker(wu.stated_slo, uint16(rps))
			l.ready = true
		}
		return s, err
	}
	return l.cb.Call(f)
}

func (l *Levee) State() State {
	return l.cb.State()
}

func (l *Levee) StateUpdates() <-chan State {
	return l.cb.StateUpdates()
}

func (l *Levee) Expunge() {
	l.ready = false
	l.cb = NewWarmupCB(l.cb.(*WarmupCB).stated_slo)
}
