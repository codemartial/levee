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

var ErrCircuitOpen = errors.New("circuit breaker is open")

// Constants
const (
	ewmaHalfLife         = 3 * time.Second
	goodputHalfLife      = 3 * time.Second
	warmupSamples        = 50
	tripBufferFactor     = 0.05
	targetSamplesPerEval = 5
	minEvalInterval      = 100 * time.Millisecond
	maxEvalInterval      = 5 * time.Second
	minInflightLimit     = 1.0
	openErrThreshold     = 0.5
	maxOpenBackoff       = 4
)

type Levee struct {
	mu  sync.Mutex
	slo SLO

	// Derived thresholds
	tripThreshold    float64
	recoverThreshold float64
	consecFailTrip   int
	cooldownDuration time.Duration

	// Circuit State
	state          State
	stateEnteredAt time.Time

	// EWMA error rate
	errEWMA     float64
	errLastTS   time.Time
	initialized bool
	samples     int64

	// Consecutive failure tracking
	consecFails int

	// Inflight tracking
	inflight      int64
	inflightLimit float64

	// Capacity signals
	goodput       float64
	avgLatency    float64
	lastSuccessTS time.Time

	// Evaluation counters
	lastEvalTS    time.Time
	evalSuccesses int64
	evalFailures  int64

	// OPEN backoff
	openStreak int
}

func deriveThresholds(slo SLO) (tripTh, recoverTh float64, consecTrip int) {
	sloErr := 1.0 - slo.SuccessRate
	consecTrip = max(int(math.Ceil(math.Log(1e-8)/math.Log(sloErr))), 5)
	recoverTh = sloErr
	if sloErr < 0.095 {
		recoverTh = 0.10
	}
	tripTh = sloErr + (slo.SuccessRate * tripBufferFactor)
	return
}

func NewLevee(slo SLO) *Levee {
	tripTh, recoverTh, consecTrip := deriveThresholds(slo)
	return &Levee{
		slo:              slo,
		tripThreshold:    tripTh,
		recoverThreshold: recoverTh,
		consecFailTrip:   consecTrip,
		cooldownDuration: slo.Timeout,
		state:            CLOSED,
		inflightLimit:    math.MaxFloat64,
	}
}

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
		if l.inflightLimit < math.MaxFloat64 {
			l.maybeRelaxLimit(ts)
		}
	case OPEN:
		shift := min(l.openStreak, maxOpenBackoff) - 1
		if ts.Sub(l.stateEnteredAt) < (l.cooldownDuration << shift) {
			return StateChange{State: OPEN}, ErrCircuitOpen
		}
		l.enterHalfOpen(ts)
		fallthrough
	case HALF_OPEN, THROTTLED:
		l.maybeEvaluateLimit(ts)
		if l.state == OPEN {
			return StateChange{State: OPEN}, ErrCircuitOpen
		}
		// At min limit, allow only one probe per eval window.
		if l.inflightLimit <= minInflightLimit && l.inflight == 0 {
			if (l.evalSuccesses + l.evalFailures) > 0 {
				return StateChange{State: l.state}, ErrCircuitOpen
			}
		}
	}

	if l.inflight >= int64(math.Ceil(l.inflightLimit)) {
		return StateChange{State: l.state}, ErrCircuitOpen
	}
	l.inflight++
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

	if l.state != OPEN && l.inflightLimit < math.MaxFloat64 {
		l.evalSuccesses++
		if l.state != CLOSED && ts.Sub(l.stateEnteredAt) >= l.effectiveRecoveryHoldoff() {
			if l.errEWMA < l.recoverThreshold || l.inflightLimit > float64(l.inflight+1)*3.0 {
				l.state = CLOSED
				l.stateEnteredAt = ts
				l.openStreak = 0
				l.resetEval(ts)
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

	if l.state != OPEN && l.inflightLimit < math.MaxFloat64 {
		l.evalFailures++
	}
	if l.state == CLOSED && l.samples >= warmupSamples {
		if l.errEWMA > l.tripThreshold || l.consecFails >= l.consecFailTrip {
			l.enterThrottled(ts)
		}
	}
	return StateChange{State: l.state}
}

func (l *Levee) updateErrEWMA(ts time.Time, sample float64) {
	if !l.initialized {
		l.errEWMA, l.errLastTS, l.initialized = sample, ts, true
		return
	}
	dt := ts.Sub(l.errLastTS).Seconds()
	l.errEWMA += l.ewmaAlpha(dt, ewmaHalfLife) * (sample - l.errEWMA)
	l.errLastTS = ts
}

func (l *Levee) updateCapacityEstimate(ts time.Time, duration time.Duration) {
	durSec := duration.Seconds()
	if l.avgLatency == 0 {
		l.avgLatency = durSec
	} else {
		dt := ts.Sub(l.lastSuccessTS).Seconds()
		l.avgLatency += l.ewmaAlpha(dt, goodputHalfLife) * (durSec - l.avgLatency)
	}

	if !l.lastSuccessTS.IsZero() {
		dt := ts.Sub(l.lastSuccessTS).Seconds()
		if dt > 0 {
			instant := 1.0 / dt
			if l.goodput == 0 {
				l.goodput = instant
			} else {
				l.goodput += l.ewmaAlpha(dt, goodputHalfLife) * (instant - l.goodput)
			}
		}
	}
	l.lastSuccessTS = ts
}

func (l *Levee) enterThrottled(ts time.Time) {
	l.state, l.stateEnteredAt = THROTTLED, ts
	l.inflightLimit = max(float64(l.inflight)*0.5, minInflightLimit)
	l.consecFails = 0
	l.resetEval(ts)
}

func (l *Levee) enterHalfOpen(ts time.Time) {
	l.state, l.stateEnteredAt = HALF_OPEN, ts
	l.inflightLimit = minInflightLimit
	l.consecFails = 0
	l.resetEval(ts)
}

func (l *Levee) resetEval(ts time.Time) {
	l.lastEvalTS, l.evalSuccesses, l.evalFailures = ts, 0, 0
}

func (l *Levee) ewmaAlpha(dt float64, halfLife time.Duration) float64 {
	if dt <= 0 {
		return 0.01
	}
	alpha := 1.0 - math.Exp(-dt*math.Ln2/halfLife.Seconds())
	if l.goodput > 0 {
		maxAlpha := 1.0 - math.Exp(-(1.0/l.goodput)*math.Ln2/halfLife.Seconds())
		alpha = min(alpha, maxAlpha)
	}
	return alpha
}

func (l *Levee) maybeRelaxLimit(ts time.Time) {
	if ts.Sub(l.lastEvalTS) < l.baseEvalInterval() {
		return
	}
	total := l.evalSuccesses + l.evalFailures
	if total > 0 && (float64(l.evalFailures)/float64(total)) > l.recoverThreshold {
		l.enterThrottled(ts)
		return
	}
	if l.inflightLimit > float64(l.inflight+1)*3.0 {
		l.inflightLimit = math.MaxFloat64
	} else {
		l.inflightLimit *= 2.0
	}
	l.resetEval(ts)
}

func (l *Levee) maybeEvaluateLimit(ts time.Time) {
	base := l.baseEvalInterval()
	effective := base
	if l.state == HALF_OPEN && l.inflightLimit <= minInflightLimit {
		effective = base << min(l.openStreak, maxOpenBackoff)
	}
	if ts.Sub(l.lastEvalTS) < effective {
		return
	}

	total := l.evalSuccesses + l.evalFailures
	if total > 0 {
		errRate := float64(l.evalFailures) / float64(total)
		if errRate > openErrThreshold && l.inflightLimit <= minInflightLimit {
			l.openStreak++
			l.state, l.stateEnteredAt, l.consecFails = OPEN, ts, 0
			return
		}
		if errRate > l.recoverThreshold {
			l.inflightLimit = max(l.inflightLimit*0.5, minInflightLimit)
		} else {
			l.inflightLimit *= math.Sqrt2
		}
	}
	l.resetEval(ts)
}

func (l *Levee) baseEvalInterval() time.Duration {
	if l.avgLatency <= 0 {
		return minEvalInterval
	}
	adaptive := time.Duration(l.avgLatency * float64(targetSamplesPerEval) * float64(time.Second))
	return min(max(adaptive, minEvalInterval), maxEvalInterval)
}

func (l *Levee) effectiveRecoveryHoldoff() time.Duration {
	return max(ewmaHalfLife, l.baseEvalInterval()*2)
}

func (l *Levee) Call(f func() error) (StateChange, error) {
	start := time.Now()
	sc, err := l.Start(start)
	if err != nil {
		return sc, err
	}
	callErr := f()
	end := time.Now()
	if callErr != nil {
		return l.Fail(end, end.Sub(start)), callErr
	}
	return l.Success(end, end.Sub(start)), nil
}

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
	OpenStreak       int     `json:"open_streak"`
}

func (l *Levee) SaveState() (*LeveeState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return &LeveeState{
		SLO: l.slo, StateVal: uint8(l.state), StateEnteredAtNS: l.stateEnteredAt.UnixNano(),
		ErrEWMA: l.errEWMA, ErrLastTSNS: l.errLastTS.UnixNano(), Initialized: l.initialized,
		Samples: l.samples, ConsecFails: l.consecFails, Inflight: l.inflight,
		InflightLimit: l.inflightLimit, Goodput: l.goodput, AvgLatency: l.avgLatency,
		LastSuccessTSNS: l.lastSuccessTS.UnixNano(), LastEvalTSNS: l.lastEvalTS.UnixNano(),
		EvalSuccesses: l.evalSuccesses, EvalFailures: l.evalFailures, OpenStreak: l.openStreak,
	}, nil
}

func RestoreState(s *LeveeState) *Levee {
	tripTh, recoverTh, consecTrip := deriveThresholds(s.SLO)
	return &Levee{
		slo: s.SLO, tripThreshold: tripTh, recoverThreshold: recoverTh, consecFailTrip: consecTrip,
		cooldownDuration: s.SLO.Timeout, state: State(s.StateVal), stateEnteredAt: time.Unix(0, s.StateEnteredAtNS),
		errEWMA: s.ErrEWMA, errLastTS: time.Unix(0, s.ErrLastTSNS), initialized: s.Initialized,
		samples: s.Samples, consecFails: s.ConsecFails, inflight: s.Inflight, inflightLimit: s.InflightLimit,
		goodput: s.Goodput, avgLatency: s.AvgLatency, lastSuccessTS: time.Unix(0, s.LastSuccessTSNS),
		lastEvalTS: time.Unix(0, s.LastEvalTSNS), evalSuccesses: s.EvalSuccesses, evalFailures: s.EvalFailures, openStreak: s.OpenStreak,
	}
}
