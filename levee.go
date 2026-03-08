package levee

import (
	"errors"
	"math"
	"sync"
	"time"
)

type SLO struct {
	SuccessRate float64
	Timeout     time.Duration
}

// State represents the circuit breaker state
type State uint8

const (
	CLOSED    State = iota
	OPEN            // Tripped: block all traffic, wait for cooldown
	THROTTLED       // Recovery: inflight-limited admission matching load to capacity
)

type Trigger error

type StateChange struct {
	State   State
	Trigger Trigger
}

type triggerError string

func (e triggerError) Error() string { return string(e) }

// ErrCircuitOpen is returned by Start when the circuit breaker rejects a request.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Constants
const (
	ewmaHalfLife     = 3 * time.Second        // ~6s effective window
	goodputHalfLife  = 3 * time.Second        // responsive capacity estimate
	warmupSamples    = 50                     // arm trip after this many observations
	tripBufferFactor = 0.05                   // tripThreshold = sloErrRate + successRate * tripBufferFactor
	evalInterval     = 500 * time.Millisecond // inflight limit re-evaluation period
	recoveryHoldoff  = 3 * time.Second        // minimum time in THROTTLED before CLOSED
	minInflightLimit = 1.0                    // always allow at least 1 request through
	openErrThreshold = 0.5                    // THROTTLED → OPEN when eval error rate exceeds this at min limit
)

// Levee is an adaptive circuit breaker and concurrency limiter.
type Levee struct {
	mu  sync.Mutex
	slo SLO

	// Derived thresholds (set once in constructor)
	sloErrRate       float64
	tripThreshold    float64
	recoverThreshold float64
	consecFailTrip   int
	cooldownDuration time.Duration

	// State
	state          State
	stateEnteredAt time.Time

	// EWMA error rate (windowed SLO signal)
	errEWMA     float64
	errLastTS   time.Time
	initialized bool
	samples     int64

	// Consecutive failure tracking (fast trip)
	consecFails int

	// Inflight tracking
	inflight      int64
	inflightLimit float64

	// Capacity estimation (Little's Law)
	goodput       float64   // successes per second (EWMA)
	avgLatency    float64   // mean success duration in seconds (EWMA)
	lastSuccessTS time.Time // for goodput inter-arrival tracking

	// THROTTLED evaluation
	lastEvalTS    time.Time
	evalSuccesses int64
	evalFailures  int64

}

func NewLevee(slo SLO) *Levee {
	sloErr := 1.0 - slo.SuccessRate
	consecThreshold := max(int(math.Ceil(math.Log(1e-8)/math.Log(sloErr))), 5)

	return &Levee{
		slo:              slo,
		sloErrRate:       sloErr,
		tripThreshold:    sloErr + slo.SuccessRate*tripBufferFactor,
		recoverThreshold: sloErr,
		consecFailTrip:   consecThreshold,
		cooldownDuration: slo.Timeout,
		state:            CLOSED,
		inflightLimit:    math.MaxFloat64,
	}
}

// State returns the current circuit breaker state.
func (l *Levee) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *Levee) Start(ts time.Time) (StateChange, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch l.state {
	case CLOSED:
		l.inflight++
		return StateChange{State: CLOSED}, nil

	case OPEN:
		if ts.Sub(l.stateEnteredAt) >= l.cooldownDuration {
			l.enterThrottledFromOpen(ts)
		} else {
			return StateChange{State: OPEN}, ErrCircuitOpen
		}
		fallthrough

	case THROTTLED:
		l.maybeEvaluateLimit(ts)
		if l.inflight >= int64(math.Ceil(l.inflightLimit)) {
			return StateChange{State: THROTTLED}, ErrCircuitOpen
		}
		l.inflight++
		return StateChange{State: THROTTLED}, nil
	}

	return StateChange{State: l.state}, nil
}

func (l *Levee) Success(ts time.Time, duration time.Duration) StateChange {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.inflight--
	l.updateErrEWMA(ts, 0.0)
	l.samples++
	l.consecFails = 0
	l.updateCapacityEstimate(ts, duration)

	if l.state == THROTTLED {
		l.evalSuccesses++
		if ts.Sub(l.stateEnteredAt) >= recoveryHoldoff {
			// Recover if error rate is low OR limit has grown well beyond usage
			if l.errEWMA < l.recoverThreshold || l.inflightLimit > float64(l.inflight+1)*3.0 {
				l.state = CLOSED
				l.stateEnteredAt = ts
				l.inflightLimit = math.MaxFloat64
			}
		}
	}

	return StateChange{State: l.state}
}

func (l *Levee) Fail(ts time.Time, duration time.Duration) StateChange {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.inflight--
	l.updateErrEWMA(ts, 1.0)
	l.samples++
	l.consecFails++

	switch l.state {
	case CLOSED:
		if l.samples >= warmupSamples {
			if l.errEWMA > l.tripThreshold || l.consecFails >= l.consecFailTrip {
				l.enterThrottledFromClosed(ts)
			}
		}
	case THROTTLED:
		l.evalFailures++
	}

	return StateChange{State: l.state}
}

// updateErrEWMA updates the error rate EWMA with a new sample (0=success, 1=failure).
func (l *Levee) updateErrEWMA(ts time.Time, sample float64) {
	if !l.initialized {
		l.errEWMA = sample
		l.errLastTS = ts
		l.initialized = true
		return
	}

	dt := ts.Sub(l.errLastTS).Seconds()
	var alpha float64
	if dt <= 0 {
		alpha = 0.01
	} else {
		alpha = 1.0 - math.Exp(-dt*math.Ln2/ewmaHalfLife.Seconds())
	}
	l.errEWMA += alpha * (sample - l.errEWMA)
	l.errLastTS = ts
}

// updateCapacityEstimate updates goodput and latency EWMAs from a success observation.
func (l *Levee) updateCapacityEstimate(ts time.Time, duration time.Duration) {
	durSec := duration.Seconds()

	if l.avgLatency == 0 {
		l.avgLatency = durSec
	} else {
		dt := ts.Sub(l.lastSuccessTS).Seconds()
		var alpha float64
		if dt <= 0 {
			alpha = 0.01
		} else {
			alpha = 1.0 - math.Exp(-dt*math.Ln2/goodputHalfLife.Seconds())
		}
		l.avgLatency += alpha * (durSec - l.avgLatency)
	}

	if !l.lastSuccessTS.IsZero() {
		dt := ts.Sub(l.lastSuccessTS).Seconds()
		if dt > 0 {
			instantGoodput := 1.0 / dt
			if l.goodput == 0 {
				l.goodput = instantGoodput
			} else {
				alpha := 1.0 - math.Exp(-dt*math.Ln2/goodputHalfLife.Seconds())
				l.goodput += alpha * (instantGoodput - l.goodput)
			}
		}
	}
	l.lastSuccessTS = ts
}

// enterThrottledFromClosed transitions from CLOSED to THROTTLED.
func (l *Levee) enterThrottledFromClosed(ts time.Time) {
	limit := max(float64(l.inflight)*0.75, minInflightLimit)

	l.state = THROTTLED
	l.stateEnteredAt = ts
	l.inflightLimit = limit
	l.lastEvalTS = ts
	l.evalSuccesses = 0
	l.evalFailures = 0
	l.consecFails = 0
}

// enterThrottledFromOpen transitions from OPEN to THROTTLED with conservative inflight limit.
func (l *Levee) enterThrottledFromOpen(ts time.Time) {
	estimatedCapacity := l.goodput * l.avgLatency
	limit := max(min(estimatedCapacity*0.25, 5), minInflightLimit)

	l.state = THROTTLED
	l.stateEnteredAt = ts
	l.inflightLimit = limit
	l.lastEvalTS = ts
	l.evalSuccesses = 0
	l.evalFailures = 0
	l.consecFails = 0
}

// enterOpen transitions to OPEN state, blocking all traffic for cooldown.
func (l *Levee) enterOpen(ts time.Time) {
	l.state = OPEN
	l.stateEnteredAt = ts
	l.consecFails = 0
}

// maybeEvaluateLimit adjusts the inflight limit using MIMD.
func (l *Levee) maybeEvaluateLimit(ts time.Time) {
	if ts.Sub(l.lastEvalTS) < evalInterval {
		return
	}

	total := l.evalSuccesses + l.evalFailures
	if total > 0 {
		evalErrRate := float64(l.evalFailures) / float64(total)

		// If error rate is catastrophically high at minimum limit, go OPEN
		// to stop failure accumulation entirely.
		if evalErrRate > openErrThreshold && l.inflightLimit <= minInflightLimit {
			l.enterOpen(ts)
			return
		}

		if evalErrRate > l.sloErrRate {
			// Multiplicative decrease
			l.inflightLimit = max(l.inflightLimit*0.5, minInflightLimit)
		} else {
			// Multiplicative increase
			l.inflightLimit *= 2.0
		}
	}

	l.evalSuccesses = 0
	l.evalFailures = 0
	l.lastEvalTS = ts
}

func (l *Levee) Call(f func() error) (StateChange, error) {
	start := time.Now()

	sc, err := l.Start(start)
	if err != nil {
		return sc, err
	}

	callErr := f()
	end := time.Now()
	duration := end.Sub(start)

	if callErr != nil {
		return l.Fail(end, duration), callErr
	}
	return l.Success(end, duration), callErr
}

// LeveeState holds the serializable state for checkpointing.
type LeveeState struct {
	SLO              SLO     `json:"slo"`
	StateVal         uint8   `json:"state"`
	StateEnteredAtNS int64   `json:"state_entered_at_ns"`
	ErrEWMA          float64 `json:"err_ewma"`
	ErrLastTSNS      int64   `json:"err_last_ts_ns"`
	Initialized      bool    `json:"initialized"`
	Samples          int64   `json:"samples"`
	ConsecFails      int     `json:"consec_fails"`
	Inflight         int64   `json:"inflight"`
	InflightLimit    float64 `json:"inflight_limit"`
	Goodput          float64 `json:"goodput"`
	AvgLatency       float64 `json:"avg_latency"`
	LastSuccessTSNS  int64   `json:"last_success_ts_ns"`
	LastEvalTSNS     int64   `json:"last_eval_ts_ns"`
	EvalSuccesses    int64   `json:"eval_successes"`
	EvalFailures     int64   `json:"eval_failures"`
}

// SaveState serializes the current state for checkpointing.
func (l *Levee) SaveState() (*LeveeState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return &LeveeState{
		SLO:              l.slo,
		StateVal:         uint8(l.state),
		StateEnteredAtNS: l.stateEnteredAt.UnixNano(),
		ErrEWMA:          l.errEWMA,
		ErrLastTSNS:      l.errLastTS.UnixNano(),
		Initialized:      l.initialized,
		Samples:          l.samples,
		ConsecFails:      l.consecFails,
		Inflight:         l.inflight,
		InflightLimit:    l.inflightLimit,
		Goodput:          l.goodput,
		AvgLatency:       l.avgLatency,
		LastSuccessTSNS:  l.lastSuccessTS.UnixNano(),
		LastEvalTSNS:     l.lastEvalTS.UnixNano(),
		EvalSuccesses:    l.evalSuccesses,
		EvalFailures:     l.evalFailures,
	}, nil
}

// RestoreState creates a Levee from a saved state.
func RestoreState(state *LeveeState) *Levee {
	sloErr := 1.0 - state.SLO.SuccessRate
	consecThreshold := max(int(math.Ceil(math.Log(1e-8)/math.Log(sloErr))), 5)

	return &Levee{
		slo:              state.SLO,
		sloErrRate:       sloErr,
		tripThreshold:    sloErr + state.SLO.SuccessRate*tripBufferFactor,
		recoverThreshold: sloErr,
		consecFailTrip:   consecThreshold,
		cooldownDuration: state.SLO.Timeout,
		state:            State(state.StateVal),
		stateEnteredAt:   time.Unix(0, state.StateEnteredAtNS),
		errEWMA:          state.ErrEWMA,
		errLastTS:        time.Unix(0, state.ErrLastTSNS),
		initialized:      state.Initialized,
		samples:          state.Samples,
		consecFails:      state.ConsecFails,
		inflight:         state.Inflight,
		inflightLimit:    state.InflightLimit,
		goodput:          state.Goodput,
		avgLatency:       state.AvgLatency,
		lastSuccessTS:    time.Unix(0, state.LastSuccessTSNS),
		lastEvalTS:       time.Unix(0, state.LastEvalTSNS),
		evalSuccesses:    state.EvalSuccesses,
		evalFailures:     state.EvalFailures,
	}
}
