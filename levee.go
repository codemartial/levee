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

type Trigger error

type StateChange struct {
	State   State
	Trigger Trigger
}

type ICircuitBreaker interface {
	// Call executes the given function and returns the state of the circuit breaker and any error
	Call(func() error) (StateChange, error)
	Start(time.Time) (StateChange, error)
	Success(time.Time, time.Duration) StateChange
	Fail(time.Time, time.Duration) StateChange
	State() State
	StateUpdates() <-chan StateChange
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

func (l *Levee) Start(ts time.Time) (StateChange, error) {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		return cb.(*WarmupCB).Start(ts)
	}
	return cb.Start(ts)
}

func (l *Levee) Success(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, true)
}

func (l *Levee) Fail(ts time.Time, duration time.Duration) StateChange {
	return l.processResult(ts, duration, false)
}

func (l *Levee) processResult(ts time.Time, duration time.Duration, success bool) StateChange {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		wu := cb.(*WarmupCB)
		var sc StateChange
		if success {
			sc = wu.Success(ts, duration)
		} else {
			sc = wu.Fail(ts, duration)
		}
		if sc.State == CLOSED {
			l.transitionFromWarmup(wu)
		}
		return sc
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
	slo := wu.slo
	wu.mu.RUnlock()

	// Figure out sample count based on SLO and detected RPS
	rps := float64(reqCount) / (end.Sub(start).Seconds() - slo.Warmup.Seconds())
	sloBasedSamples := 10 / (1 - slo.SuccessRate)
	samples := max(rps, 100, sloBasedSamples)
	samples = min(samples, 1<<16-1)
	l.cb = NewCircuitBreaker(slo, uint16(samples))
	l.ready = true
}

func (l *Levee) Call(f func() error) (StateChange, error) {
	l.mu.RLock()
	ready := l.ready
	cb := l.cb
	l.mu.RUnlock()

	if !ready {
		wu := cb.(*WarmupCB)
		sc, err := wu.Call(f)
		if sc.State == CLOSED {
			l.transitionFromWarmup(wu)
		}
		return sc, err
	}
	return cb.Call(f)
}

func (l *Levee) State() State {
	return l.cb.State()
}

func (l *Levee) StateUpdates() <-chan StateChange {
	return l.cb.StateUpdates()
}

func (l *Levee) Expunge() {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Get SLO from current CB (could be WarmupCB or CircuitBreaker)
	var slo SLO
	if wu, ok := l.cb.(*WarmupCB); ok {
		slo = wu.slo
	} else if cb, ok := l.cb.(*CircuitBreaker); ok {
		cb.mu.RLock()
		slo = cb.stated_slo
		cb.mu.RUnlock()
	}

	l.ready = false
	l.cb = NewWarmupCB(slo)
}
