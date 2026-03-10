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
	THROTTLED       // SLO breach: inflight-limited admission, MIMD toward capacity
	HALF_OPEN       // Post-OPEN probe: conservative admission, adaptive eval interval
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
	ewmaHalfLife     = 3 * time.Second // ~6s effective window
	goodputHalfLife  = 3 * time.Second // responsive capacity estimate
	warmupSamples    = 50              // arm trip after this many observations
	tripBufferFactor = 0.05            // tripThreshold = sloErrRate + successRate * tripBufferFactor

	// Eval interval adapts to observed latency: avgLatency * targetSamplesPerEval,
	// clamped to [minEvalInterval, maxEvalInterval]. At 100ms latency this produces
	// 500ms — same as the original fixed value.
	targetSamplesPerEval = 5
	minEvalInterval      = 100 * time.Millisecond
	maxEvalInterval      = 5 * time.Second

	// Recovery holdoff = baseEvalInterval * holdoffEvalMultiplier, floored.
	// At 100ms latency: 500ms * 6 = 3s — same as the original fixed value.
	holdoffEvalMultiplier = 6
	minRecoveryHoldoff    = 1 * time.Second

	minInflightLimit = 1.0 // always allow at least 1 request through
	openErrThreshold = 0.5 // THROTTLED → OPEN when eval error rate exceeds this at min limit
	maxOpenBackoff   = 4   // max OPEN cooldown exponent: 2^(4-1) = 8× base cooldown
)

// Levee is an adaptive circuit breaker and concurrency limiter.
type Levee struct {
	mu  sync.Mutex
	slo SLO

	// Derived thresholds (set once in constructor)
	sloErrRate       float64
	tripThreshold    float64
	recoverThreshold float64 // max(sloErrRate, floor) — operational threshold for MIMD and recovery
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

	// OPEN backoff
	openStreak int // consecutive OPEN entries without full CLOSED recovery

}

func NewLevee(slo SLO) *Levee {
	sloErr := 1.0 - slo.SuccessRate
	consecThreshold := max(int(math.Ceil(math.Log(1e-8)/math.Log(sloErr))), 5)

	// For tight SLOs (error rate < ~10%), use a floor of 10% for MIMD/recovery
	// thresholds to prevent over-blocking. The 0.095 cutoff avoids IEEE 754
	// float issues at SLO=0.90 where 1.0-0.9 = 0.09999999999999998.
	recoverTh := sloErr
	if sloErr < 0.095 {
		recoverTh = 0.10
	}

	return &Levee{
		slo:              slo,
		sloErrRate:       sloErr,
		tripThreshold:    sloErr + slo.SuccessRate*tripBufferFactor,
		recoverThreshold: recoverTh,
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
		// If recovering with active limit, grow it via MIMD
		if l.inflightLimit < math.MaxFloat64 {
			l.maybeRelaxLimit(ts)
			if l.inflight >= int64(math.Ceil(l.inflightLimit)) {
				return StateChange{State: CLOSED}, ErrCircuitOpen
			}
		}
		l.inflight++
		return StateChange{State: CLOSED}, nil

	case OPEN:
		shift := min(l.openStreak, maxOpenBackoff) - 1
		dynamicCooldown := l.cooldownDuration << shift
		if ts.Sub(l.stateEnteredAt) >= dynamicCooldown {
			l.enterHalfOpen(ts)
		} else {
			return StateChange{State: OPEN}, ErrCircuitOpen
		}
		fallthrough

	case HALF_OPEN, THROTTLED:
		l.maybeEvaluateLimit(ts)
		if l.state == OPEN {
			return StateChange{State: OPEN}, ErrCircuitOpen
		}
		// At minimum limit, allow only one probe per eval window.
		// This adapts probe frequency to the eval interval, which itself
		// scales with openStreak — reducing aggregate probe pressure
		// when many instances share a degraded backend.
		if l.inflightLimit <= minInflightLimit && l.inflight == 0 {
			if (l.evalSuccesses + l.evalFailures) > 0 {
				return StateChange{State: l.state}, ErrCircuitOpen
			}
		}
		if l.inflight >= int64(math.Ceil(l.inflightLimit)) {
			return StateChange{State: l.state}, ErrCircuitOpen
		}
		l.inflight++
		return StateChange{State: l.state}, nil
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

	switch l.state {
	case CLOSED:
		// Track eval counters for active limit in CLOSED
		if l.inflightLimit < math.MaxFloat64 {
			l.evalSuccesses++
		}
	case THROTTLED, HALF_OPEN:
		l.evalSuccesses++
		if ts.Sub(l.stateEnteredAt) >= l.effectiveRecoveryHoldoff() {
			// Recover if error rate is low OR limit has grown well beyond usage
			if l.errEWMA < l.recoverThreshold || l.inflightLimit > float64(l.inflight+1)*3.0 {
				l.state = CLOSED
				l.stateEnteredAt = ts
				l.openStreak = 0
				// Keep inflightLimit — it will grow via maybeRelaxLimit in CLOSED
				l.lastEvalTS = ts
				l.evalSuccesses = 0
				l.evalFailures = 0
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
		if l.inflightLimit < math.MaxFloat64 {
			l.evalFailures++
		}
		if l.samples >= warmupSamples {
			if l.errEWMA > l.tripThreshold || l.consecFails >= l.consecFailTrip {
				l.enterThrottled(ts)
			}
		}
	case THROTTLED, HALF_OPEN:
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
	alpha := l.ewmaAlpha(dt, ewmaHalfLife)
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
		alpha := l.ewmaAlpha(dt, goodputHalfLife)
		l.avgLatency += alpha * (durSec - l.avgLatency)
	}

	if !l.lastSuccessTS.IsZero() {
		dt := ts.Sub(l.lastSuccessTS).Seconds()
		if dt > 0 {
			instantGoodput := 1.0 / dt
			if l.goodput == 0 {
				l.goodput = instantGoodput
			} else {
				alpha := l.ewmaAlpha(dt, goodputHalfLife)
				l.goodput += alpha * (instantGoodput - l.goodput)
			}
		}
	}
	l.lastSuccessTS = ts
}

// enterThrottled transitions from CLOSED to THROTTLED on SLO breach.
func (l *Levee) enterThrottled(ts time.Time) {
	limit := max(float64(l.inflight)*0.5, minInflightLimit)

	l.state = THROTTLED
	l.stateEnteredAt = ts
	l.inflightLimit = limit
	l.lastEvalTS = ts
	l.evalSuccesses = 0
	l.evalFailures = 0
	l.consecFails = 0
}

// enterHalfOpen transitions from OPEN to HALF_OPEN with conservative inflight limit.
func (l *Levee) enterHalfOpen(ts time.Time) {
	estimatedCapacity := l.goodput * l.avgLatency
	limit := max(min(estimatedCapacity*0.25, 1), minInflightLimit)

	l.state = HALF_OPEN
	l.stateEnteredAt = ts
	l.inflightLimit = limit
	l.lastEvalTS = ts
	l.evalSuccesses = 0
	l.evalFailures = 0
	l.consecFails = 0
}

// baseEvalInterval returns the adaptive eval interval based on observed latency.
// At 100ms latency with targetSamplesPerEval=5, this produces 500ms.
func (l *Levee) baseEvalInterval() time.Duration {
	if l.avgLatency <= 0 {
		return minEvalInterval
	}
	adaptive := time.Duration(l.avgLatency * float64(targetSamplesPerEval) * float64(time.Second))
	if adaptive < minEvalInterval {
		return minEvalInterval
	}
	if adaptive > maxEvalInterval {
		return maxEvalInterval
	}
	return adaptive
}

// effectiveRecoveryHoldoff returns the recovery holdoff scaled to the eval interval.
// At 100ms latency: 500ms * 6 = 3s.
func (l *Levee) effectiveRecoveryHoldoff() time.Duration {
	holdoff := l.baseEvalInterval() * holdoffEvalMultiplier
	if holdoff < minRecoveryHoldoff {
		return minRecoveryHoldoff
	}
	return holdoff
}

// ewmaAlpha computes a time-weighted EWMA alpha, capped so that idle periods
// (dt >> inter-observation interval) don't erase history in a single update.
func (l *Levee) ewmaAlpha(dt float64, halfLife time.Duration) float64 {
	if dt <= 0 {
		return 0.01
	}
	alpha := 1.0 - math.Exp(-dt*math.Ln2/halfLife.Seconds())
	if l.goodput > 0 {
		maxDt := 1.0 / l.goodput
		maxAlpha := 1.0 - math.Exp(-maxDt*math.Ln2/halfLife.Seconds())
		if alpha > maxAlpha {
			alpha = maxAlpha
		}
	}
	return alpha
}

// enterOpen transitions to OPEN state, blocking all traffic for cooldown.
// Each consecutive OPEN without full recovery doubles the cooldown (exponential backoff).
func (l *Levee) enterOpen(ts time.Time) {
	l.openStreak++
	l.state = OPEN
	l.stateEnteredAt = ts
	l.consecFails = 0
}

// maybeRelaxLimit grows the inflight limit in CLOSED state after recovery.
// If the limit is already well above actual usage (low-traffic or cooperative scenario),
// relaxes immediately. Otherwise grows via 2x MIMD to prevent post-recovery burst.
func (l *Levee) maybeRelaxLimit(ts time.Time) {
	if ts.Sub(l.lastEvalTS) < l.baseEvalInterval() {
		return
	}

	total := l.evalSuccesses + l.evalFailures
	if total > 0 {
		evalErrRate := float64(l.evalFailures) / float64(total)
		if evalErrRate > l.recoverThreshold {
			// Errors rising during recovery — trip to THROTTLED immediately
			l.enterThrottled(ts)
			return
		}
	}

	// If limit is already not constraining traffic, relax immediately.
	// This helps cooperative scenarios where per-instance inflight is low.
	if l.inflightLimit > float64(l.inflight+1)*3.0 {
		l.inflightLimit = math.MaxFloat64
	} else {
		// Good eval window — double the limit
		l.inflightLimit *= 2.0
	}

	l.evalSuccesses = 0
	l.evalFailures = 0
	l.lastEvalTS = ts
}

// maybeEvaluateLimit adjusts the inflight limit using MIMD.
// In HALF_OPEN at minimum limit, the eval interval scales exponentially
// with openStreak — adapting probe frequency to failure history.
// Note: this uses the same maxOpenBackoff cap and exponential base as the
// OPEN cooldown in Start(). The coupling is intentional — both mechanisms
// should back off in lockstep — but changing one requires reviewing the other.
func (l *Levee) maybeEvaluateLimit(ts time.Time) {
	base := l.baseEvalInterval()
	effectiveInterval := base
	if l.state == HALF_OPEN && l.inflightLimit <= minInflightLimit {
		shift := min(l.openStreak, maxOpenBackoff)
		effectiveInterval = base << shift
	}
	if ts.Sub(l.lastEvalTS) < effectiveInterval {
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

		if evalErrRate > l.recoverThreshold {
			// Multiplicative decrease
			l.inflightLimit = max(l.inflightLimit*0.5, minInflightLimit)
		} else {
			// Multiplicative increase
			l.inflightLimit *= math.Sqrt2
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
	OpenStreak int `json:"open_streak"`
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
		OpenStreak: l.openStreak,
	}, nil
}

// RestoreState creates a Levee from a saved state.
func RestoreState(state *LeveeState) *Levee {
	sloErr := 1.0 - state.SLO.SuccessRate
	consecThreshold := max(int(math.Ceil(math.Log(1e-8)/math.Log(sloErr))), 5)

	recoverTh := sloErr
	if sloErr < 0.095 {
		recoverTh = 0.10
	}

	return &Levee{
		slo:              state.SLO,
		sloErrRate:       sloErr,
		tripThreshold:    sloErr + state.SLO.SuccessRate*tripBufferFactor,
		recoverThreshold: recoverTh,
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
		openStreak: state.OpenStreak,
	}
}
