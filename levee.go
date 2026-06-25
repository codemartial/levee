package levee

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"time"
)

// SLO is the service-level objective a [Levee] defends. All of a Levee's
// behaviour is self-tuned from these two fields; there are no other knobs.
type SLO struct {
	// SuccessRate is the target fraction of successful calls, exclusive in
	// (0, 1). For example 0.95 means "tolerate up to a 5% error rate". A Levee
	// trips only once it is statistically confident the budget is breached.
	SuccessRate float64
	// Timeout is the base cooldown applied while the circuit is OPEN before the
	// next probe. It must be > 0. Repeated trips back off exponentially from it.
	Timeout time.Duration
}

// State is the admission state of a [Levee].
type State uint8

const (
	// CLOSED is the healthy state: all traffic is admitted uncapped.
	CLOSED State = iota
	// OPEN is the tripped state: all traffic is rejected with [ErrCircuitOpen]
	// until the cooldown elapses and the breaker probes via HALF_OPEN.
	OPEN
	// THROTTLED is the overload state: admission is capped by an adaptive
	// inflight limit that converges toward observed capacity (MIMD control).
	THROTTLED
	// HALF_OPEN is the post-OPEN probe state: admission resumes at the minimum
	// inflight limit on an adaptive evaluation interval to test recovery.
	HALF_OPEN
)

// String returns the state's name, e.g. "CLOSED".
func (s State) String() string {
	switch s {
	case CLOSED:
		return "CLOSED"
	case OPEN:
		return "OPEN"
	case THROTTLED:
		return "THROTTLED"
	case HALF_OPEN:
		return "HALF_OPEN"
	default:
		return "State(" + strconv.Itoa(int(s)) + ")"
	}
}

// Trigger describes why a state change happened. It is reserved for future use:
// it is currently always nil, and callers must not depend on it being set.
type Trigger error

// StateChange is returned by every admission and completion call to report the
// circuit state observed after the call.
type StateChange struct {
	// State is the circuit state after the call.
	State State
	// Trigger is reserved for future use and is currently always nil.
	Trigger Trigger
}

// ErrCircuitOpen is returned by [Levee.Start] (and [Levee.Call]) when a request
// is rejected, either because the circuit is OPEN or because the adaptive
// inflight limit is already saturated.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// Constants
const (
	ewmaHalfLife         = 3 * time.Second
	goodputHalfLife      = 3 * time.Second
	warmupSamples        = 50
	targetSamplesPerEval = 5
	minEvalInterval      = 100 * time.Millisecond
	maxEvalInterval      = 5 * time.Second
	minInflightLimit     = 1.0
	openErrThreshold     = 0.5
	maxOpenBackoff       = 4
	warmupConsecTrip     = 5 // consecutive failures that trip even during warmup (cold-start outage)
)

// Levee is a self-tuning circuit breaker and concurrency limiter. The zero value
// is not usable; construct one with [NewLevee] or [RestoreState]. All methods are
// safe for concurrent use.
type Levee struct {
	mu  sync.Mutex
	slo SLO

	// Derived thresholds
	tripZ            float64 // one-sided z for the trip confidence bound, = invNormCDF(SuccessRate)
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

func deriveThresholds(slo SLO) (tripZ, recoverTh float64, consecTrip int) {
	sloErr := 1.0 - slo.SuccessRate
	consecTrip = int(math.Ceil(math.Log(1e-8) / math.Log(sloErr)))
	recoverTh = sloErr
	if sloErr < 0.095 {
		recoverTh = 0.10
	}
	// Confidence to trip = the SLO: a 0.99 SLO trips only when 99% sure the
	// budget is breached. Floor at 0.5 (z >= 0) for the degenerate SLO < 0.5.
	conf := slo.SuccessRate
	if conf < 0.5 {
		conf = 0.5
	}
	tripZ = invNormCDF(conf)
	return
}

// inverse standard-normal CDF (probit), used once at construction to turn the
// SLO success rate into the trip confidence z.
func invNormCDF(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	return math.Sqrt2 * math.Erfinv(2*p-1)
}

func validateSLO(slo SLO) {
	if math.IsNaN(slo.SuccessRate) || slo.SuccessRate <= 0 || slo.SuccessRate >= 1 {
		panic("levee: SLO.SuccessRate must be > 0 and < 1")
	}
	if slo.Timeout <= 0 {
		panic("levee: SLO.Timeout must be > 0")
	}
}

// NewLevee returns a Levee that defends the given [SLO], starting in the CLOSED
// state. It panics if the SLO is invalid (SuccessRate not in (0, 1), or
// Timeout <= 0), since that is a programming error rather than a runtime fault.
func NewLevee(slo SLO) *Levee {
	validateSLO(slo)
	tripZ, recoverTh, consecTrip := deriveThresholds(slo)
	return &Levee{
		slo:              slo,
		tripZ:            tripZ,
		recoverThreshold: recoverTh,
		consecFailTrip:   consecTrip,
		cooldownDuration: slo.Timeout,
		state:            CLOSED,
		inflightLimit:    math.MaxFloat64,
	}
}

// State returns the current circuit [State].
func (l *Levee) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// Start requests admission for one call at time ts. If it returns a nil error the
// call was admitted and the caller must report its outcome exactly once with
// [Levee.Success] or [Levee.Fail]. If it returns [ErrCircuitOpen] the call was
// rejected and must not be reported. ts is the caller's clock, letting the breaker
// be driven from a real, simulated, or externally-sourced time.
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

	if l.inflightLimit < math.MaxFloat64 && l.inflight >= int64(math.Ceil(l.inflightLimit)) {
		return StateChange{State: l.state}, ErrCircuitOpen
	}
	l.inflight++
	return StateChange{State: l.state}, nil
}

// Success reports that an admitted call completed successfully at time ts after
// taking the given duration. Call it exactly once per call that [Levee.Start]
// admitted with a nil error.
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
				// restart warmup if we tripped during warmup
				if l.samples < warmupSamples {
					l.samples = 0
					l.errEWMA = 0.0
				}
			}
		}
	}
	return StateChange{State: l.state}
}

// Fail reports that an admitted call failed at time ts after taking the given
// duration. Call it exactly once per admitted call. Sustained failures move the
// breaker out of CLOSED toward THROTTLED and, if they persist, OPEN.
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
	if l.state == CLOSED {
		consecThresh := l.consecFailTrip
		// Use a more conservative threshold during warmup
		if l.samples < warmupSamples && consecThresh > warmupConsecTrip {
			consecThresh = warmupConsecTrip
		}
		sloErr := 1.0 - l.slo.SuccessRate
		if (l.samples >= warmupSamples && l.errLowerBound() > sloErr) || l.consecFails >= consecThresh {
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
	// Seed the limit from observed capacity
	capacity := l.goodput * l.avgLatency
	l.inflightLimit = max(max(capacity, float64(l.inflight))*0.5, minInflightLimit)
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

// requestRate estimates total events/sec, recovered from the success-only
// goodput EWMA (denominator guarded near a full outage).
func (l *Levee) requestRate() float64 {
	return l.goodput / max(1.0-l.errEWMA, 0.05)
}

// effectiveSamples is errEWMA's sample mass: ~2 half-lives of traffic.
func (l *Levee) effectiveSamples() float64 {
	return max(2.0*l.requestRate()*ewmaHalfLife.Seconds()/math.Ln2, 1.0)
}

// errLowerBound is the Wilson lower bound on the error rate at confidence tripZ.
func (l *Levee) errLowerBound() float64 {
	p := l.errEWMA
	if l.goodput <= 0 {
		return p
	}
	n := l.effectiveSamples()
	z2 := l.tripZ * l.tripZ
	center := p + z2/(2.0*n)
	margin := l.tripZ * math.Sqrt(p*(1.0-p)/n+z2/(4.0*n*n))
	return (center - margin) / (1.0 + z2/n)
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

// Call is the in-band convenience wrapper: it admits a call, runs f if admitted,
// and reports the outcome, timing f with the wall clock. If admission is rejected
// it returns [ErrCircuitOpen] without running f. Otherwise it returns f's error
// (if any). Use the explicit [Levee.Start]/[Levee.Success]/[Levee.Fail] API when
// you need to control timing or run work out of band.
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

// LeveeState is a serializable snapshot of a [Levee], produced by
// [Levee.SaveState] and consumed by [RestoreState]. It carries the learned
// signals (error EWMA, capacity estimate, inflight limit, circuit state) so a
// restarted process can resume without re-learning from a cold start. The live
// inflight count is intentionally not restored.
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

// SaveState returns a snapshot of the breaker suitable for serialization (for
// example as JSON). The error is always nil today; it is part of the signature so
// future encoding work can fail without a breaking API change.
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

// RestoreState rebuilds a Levee from a snapshot taken by [Levee.SaveState]. The
// live inflight count is deliberately dropped (reset to zero): the calls it
// represented did not survive the restart, so counting them would wedge admission.
// It panics if s is nil or carries an invalid SLO.
func RestoreState(s *LeveeState) *Levee {
	if s == nil {
		panic("levee: nil LeveeState")
	}
	validateSLO(s.SLO)
	tripZ, recoverTh, consecTrip := deriveThresholds(s.SLO)
	return &Levee{
		slo: s.SLO, tripZ: tripZ, recoverThreshold: recoverTh, consecFailTrip: consecTrip,
		cooldownDuration: s.SLO.Timeout, state: State(s.StateVal), stateEnteredAt: time.Unix(0, s.StateEnteredAtNS),
		errEWMA: s.ErrEWMA, errLastTS: time.Unix(0, s.ErrLastTSNS), initialized: s.Initialized,
		samples: s.Samples, consecFails: s.ConsecFails, inflightLimit: s.InflightLimit,
		goodput: s.Goodput, avgLatency: s.AvgLatency, lastSuccessTS: time.Unix(0, s.LastSuccessTSNS),
		lastEvalTS: time.Unix(0, s.LastEvalTSNS), evalSuccesses: s.EvalSuccesses, evalFailures: s.EvalFailures, openStreak: s.OpenStreak,
	}
}
