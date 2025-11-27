package levee

import (
	"sync"
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
	Start(time.Time) (State, error)
	Success(time.Time, time.Duration) State
	Fail(time.Time, time.Duration) State
	State() State
	StateUpdates() <-chan State
}

type Levee struct {
	mu    sync.RWMutex
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

func (l *Levee) Start(ts time.Time) (State, error) {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		return cb.(*WarmupCB).Start(ts)
	}
	return cb.Start(ts)
}

func (l *Levee) Success(ts time.Time, duration time.Duration) State {
	return l.processResult(ts, duration, true)
}

func (l *Levee) Fail(ts time.Time, duration time.Duration) State {
	return l.processResult(ts, duration, false)
}

func (l *Levee) processResult(ts time.Time, duration time.Duration, success bool) State {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		wu := cb.(*WarmupCB)
		var s State
		if success {
			s = wu.Success(ts, duration)
		} else {
			s = wu.Fail(ts, duration)
		}
		if s == CLOSED {
			l.transitionFromWarmup(wu)
		}
		return s
	}

	if success {
		return cb.Success(ts, duration)
	}
	return cb.Fail(ts, duration)
}

func (l *Levee) transitionFromWarmup(wu *WarmupCB) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Check again with write lock to prevent duplicate transitions
	if l.ready {
		return
	}

	// Read warmup metrics with proper locking
	wu.mu.RLock()
	reqCount := wu.reqCount
	start := wu.start
	end := wu.end
	slo := wu.stated_slo
	wu.mu.RUnlock()

	// Figure out sample count based on SLO and detected RPS
	rps := float64(reqCount) / (end.Sub(start).Seconds() - slo.Warmup.Seconds())
	sloBasedSamples := 10 / (1 - slo.SuccessRate)
	samples := max(rps, 100, sloBasedSamples)
	samples = min(samples, 1<<16-1)
	l.cb = NewCircuitBreaker(slo, uint16(samples))
	l.ready = true
}

func (l *Levee) Call(f func() error) (State, error) {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		wu := cb.(*WarmupCB)
		s, err := wu.Call(f)
		if s == CLOSED {
			l.transitionFromWarmup(wu)
		}
		return s, err
	}
	return cb.Call(f)
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
