package levee

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SLO is the service-level objective a [Levee] defends. All of a Levee's
// behaviour is self-tuned from these two fields; there are no other knobs.
type SLO struct {
	// SuccessRate is the target success fraction, in (0, 1) exclusive. The breaker
	// trips only when statistically confident the budget is breached.
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

// Trigger describes why a state change happened.
type Trigger uint8

const (
	// TriggerNone means the call did not cause a state transition.
	TriggerNone Trigger = iota
	// TriggerFailureRate means statistical failure evidence exceeded the SLO's
	// error budget and caused throttling.
	TriggerFailureRate
	// TriggerConsecutiveFailures means a run of failures caused throttling.
	TriggerConsecutiveFailures
	// TriggerSurge means proactive surge strain caused throttling.
	TriggerSurge
	// TriggerMinLimitFailureRate means failures remained excessive at the
	// minimum inflight limit, causing the circuit to open.
	TriggerMinLimitFailureRate
	// TriggerCooldownExpired means the open cooldown elapsed and a recovery
	// probe moved the circuit to half-open.
	TriggerCooldownExpired
	// TriggerRecovered means healthy evidence moved a limited circuit back to
	// closed.
	TriggerRecovered
)

// String returns the trigger's name, e.g. "FAILURE_RATE".
func (t Trigger) String() string {
	switch t {
	case TriggerNone:
		return "NONE"
	case TriggerFailureRate:
		return "FAILURE_RATE"
	case TriggerConsecutiveFailures:
		return "CONSECUTIVE_FAILURES"
	case TriggerSurge:
		return "SURGE"
	case TriggerMinLimitFailureRate:
		return "MIN_LIMIT_FAILURE_RATE"
	case TriggerCooldownExpired:
		return "COOLDOWN_EXPIRED"
	case TriggerRecovered:
		return "RECOVERED"
	default:
		return "Trigger(" + strconv.Itoa(int(t)) + ")"
	}
}

// StateChange is returned by every admission and completion call to report the
// circuit state observed after the call.
type StateChange struct {
	// State is the circuit state after the call.
	State State
	// Trigger describes a state transition caused by this call. It is
	// [TriggerNone] when the call did not cause a transition.
	Trigger Trigger
}

// Snapshot is a read-only point-in-time view of a Levee's controller signals.
// It is intended for diagnostics and metrics, not persistence or tuning.
type Snapshot struct {
	// State is the current circuit state.
	State State
	// Trigger is the most recent non-zero transition trigger.
	Trigger Trigger
	// Inflight is the number of admitted calls not yet completed.
	Inflight int64
	// Capped reports whether an inflight admission limit is active.
	Capped bool
	// Limit is the effective integer admission limit when Capped is true, and
	// zero when admission is uncapped.
	Limit int64
	// EstimatedCapacity is the estimated healthy concurrency: goodput times
	// average latency.
	EstimatedCapacity float64
	// ErrorRate is the current error-rate EWMA.
	ErrorRate float64
	// ErrorLowerBound is the Wilson lower confidence bound used for tripping.
	ErrorLowerBound float64
	// Surge contains the proactive surge controller's diagnostic state.
	Surge SurgeSnapshot
}

// SurgeSnapshot is a read-only view of proactive surge protection.
type SurgeSnapshot struct {
	// Armed reports whether excess inflight is currently loading the surge
	// spring.
	Armed bool
	// Onset is the published inflight level beyond which surge strain begins to
	// accumulate, or zero while surge protection is unpublished.
	Onset int64
	// Strain is current surge strain divided by its trip budget. It may exceed
	// one while remembered strain is carried through recovery.
	Strain float64
	// ProvenExcess is healthy concurrency proven beyond the lagging EWMA
	// capacity estimate.
	ProvenExcess float64
}

// ErrCircuitOpen is returned by [Levee.Start] and [Levee.Call] when a request is
// rejected (the circuit is OPEN or the inflight limit is saturated).
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

	// healthyTailProb is the shared "effectively never under health" rarity: it
	// sizes both the consecutive-failure trip and the surge onset quantile.
	healthyTailProb = 1e-8
)

// surgeOnsetZ is the one-sided normal quantile at healthyTailProb, used by
// surgeOnset to bound the Poisson tail of healthy inflight.
var surgeOnsetZ = invNormCDF(1 - healthyTailProb)

// Levee is a self-tuning circuit breaker and concurrency limiter. Construct one
// with [NewLevee]; the zero value is unusable. All methods are concurrency-safe.
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
	lastTrigger    Trigger     // most recent state transition; diagnostics only
	capped         atomic.Bool // admission gate: false == uncapped; read lock-free by Start
	surgeArmed     bool        // surge spring loaded; proof window start/count live in lastEvalTS/evalSuccesses
	stateEnteredAt time.Time

	// EWMA error rate; errLastTS.IsZero() means no sample seen yet
	errEWMA   float64
	errLastTS time.Time
	samples   int64

	// Consecutive failure tracking
	consecFails int

	// inflight is incremented lock-free (atomic); inflightLimit is the active cap,
	// plain because only the mutex-guarded slow path touches it.
	inflight      atomic.Int64
	inflightLimit float64

	// Capacity signals
	goodput       float64
	avgLatency    float64
	lastSuccessTS time.Time

	// Surge spring: proactive trip out of uncapped CLOSED when inflight
	// stretches past the published onset. Strain integrates the stretch over
	// time and relaxes under proof of health (see surgeTick). The proof window
	// borrows the eval fields below: the eval window runs only while capped,
	// arming happens only while uncapped-CLOSED, and every capped transition
	// disarms and resets eval, so the two never coexist.
	surgeLimit    atomic.Int64 // published stretch onset; 0 = disarmed; read lock-free by Start
	surgeLatSnap  float64      // avgLatency snapshot taken when armed, before queueing pollutes it
	surgeStrain   float64      // integrated stretch-seconds; trips at surgeStrainBudget
	surgeStrainTS time.Time    // last strain integration point; frozen across capped spans
	surgeProven   float64      // capacity proven past the EWMA estimate; decays at goodputHalfLife

	// Evaluation window: capped-limit adjustment while capped, surge
	// confirmation while armed (see surge invariant above).
	lastEvalTS    time.Time
	evalSuccesses int64
	evalFailures  int64

	// OPEN backoff
	openStreak int
}

func deriveThresholds(slo SLO) (tripZ, recoverTh float64, consecTrip int) {
	sloErr := 1.0 - slo.SuccessRate
	consecTrip = int(math.Ceil(math.Log(healthyTailProb) / math.Log(sloErr)))
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

// NewLevee returns a Levee defending the given [SLO], starting CLOSED. It panics on
// an invalid SLO (SuccessRate not in (0, 1), or Timeout <= 0).
func NewLevee(slo SLO) *Levee {
	validateSLO(slo)
	tripZ, recoverTh, consecTrip := deriveThresholds(slo)
	// capped and inflightLimit default to the uncapped, admit-everything state.
	return &Levee{
		slo:              slo,
		tripZ:            tripZ,
		recoverThreshold: recoverTh,
		consecFailTrip:   consecTrip,
		cooldownDuration: slo.Timeout,
		state:            CLOSED,
	}
}

// State returns the current circuit [State].
func (l *Levee) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// Snapshot returns a read-only point-in-time view of the controller. It takes
// the controller mutex because it is a diagnostic path; healthy admission is
// unaffected. Under concurrent traffic, Inflight may change immediately after
// the snapshot is returned.
func (l *Levee) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	capped := l.capped.Load()
	var limit int64
	if capped {
		limit = int64(math.Ceil(l.inflightLimit))
	}
	budget := l.surgeStrainBudget()
	strain := 0.0
	if budget > 0 {
		strain = l.surgeStrain / budget
	}
	return Snapshot{
		State:             l.state,
		Trigger:           l.lastTrigger,
		Inflight:          l.inflight.Load(),
		Capped:            capped,
		Limit:             limit,
		EstimatedCapacity: l.goodput * l.avgLatency,
		ErrorRate:         l.errEWMA,
		ErrorLowerBound:   l.errLowerBound(),
		Surge: SurgeSnapshot{
			Armed:        l.surgeArmed,
			Onset:        l.surgeLimit.Load(),
			Strain:       strain,
			ProvenExcess: l.surgeProven,
		},
	}
}

// transition records an actual state transition and its diagnostic cause.
func (l *Levee) transition(state State, trigger Trigger, ts time.Time) {
	l.state = state
	l.stateEnteredAt = ts
	l.lastTrigger = trigger
}

// stateChange reports a trigger only when this public call changed state.
func (l *Levee) stateChange(previous State) StateChange {
	change := StateChange{State: l.state}
	if l.state != previous {
		change.Trigger = l.lastTrigger
	}
	return change
}

// Start requests admission at time ts. A nil error means admitted (report it once
// via [Levee.Success] or [Levee.Fail]); [ErrCircuitOpen] means rejected, report nothing.
// It is the caller's responsibility to ensure that timestamps are coherent, e.g. from
// the clock on the machine that these calls are being made. See [Levee.Call], for example.
func (l *Levee) Start(ts time.Time) (StateChange, error) {
	// Lock-free fast path: an uncapped breaker (only possible while CLOSED) admits
	// everything with just atomic accounting, never touching the mutex.
	if !l.capped.Load() {
		n := l.inflight.Add(1)
		if lim := l.surgeLimit.Load(); lim > 0 && n > lim {
			return l.surgeCheck(ts)
		}
		return StateChange{State: CLOSED}, nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.state

	switch l.state {
	case CLOSED:
		if l.capped.Load() {
			l.maybeRelaxLimit(ts)
		}
	case OPEN:
		shift := min(l.openStreak, maxOpenBackoff) - 1
		if ts.Sub(l.stateEnteredAt) < (l.cooldownDuration << shift) {
			return l.stateChange(previous), ErrCircuitOpen
		}
		l.enterHalfOpen(ts)
		fallthrough
	case HALF_OPEN, THROTTLED:
		l.maybeEvaluateLimit(ts)
		if l.state == OPEN {
			return l.stateChange(previous), ErrCircuitOpen
		}
		// At min limit, allow only one probe per eval window.
		if l.inflightLimit <= minInflightLimit && l.inflight.Load() == 0 {
			if (l.evalSuccesses + l.evalFailures) > 0 {
				return l.stateChange(previous), ErrCircuitOpen
			}
		}
	}

	if l.capped.Load() && l.inflight.Load() >= int64(math.Ceil(l.inflightLimit)) {
		return l.stateChange(previous), ErrCircuitOpen
	}
	l.inflight.Add(1)
	return l.stateChange(previous), nil
}

// Success reports that an admitted call succeeded at time ts after the given
// duration. Call it exactly once per admission.
func (l *Levee) Success(ts time.Time, duration time.Duration) StateChange {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.state

	l.inflight.Add(-1)
	l.updateErrEWMA(ts, 0.0)
	l.samples++
	l.consecFails = 0
	l.updateCapacityEstimate(ts, duration)
	l.surgeOnCompletion(ts)

	if l.state != OPEN && l.capped.Load() {
		l.evalSuccesses++
		if l.state != CLOSED && ts.Sub(l.stateEnteredAt) >= l.effectiveRecoveryHoldoff() {
			if l.errEWMA < l.recoverThreshold || l.inflightLimit > float64(l.inflight.Load()+1)*3.0 {
				l.transition(CLOSED, TriggerRecovered, ts)
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
	return l.stateChange(previous)
}

// Fail reports that an admitted call failed at time ts after the given duration.
// Call it exactly once per admission.
func (l *Levee) Fail(ts time.Time, duration time.Duration) StateChange {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.state

	l.inflight.Add(-1)
	l.updateErrEWMA(ts, 1.0)
	l.samples++
	l.consecFails++
	l.surgeOnCompletion(ts)

	if l.state != OPEN && l.capped.Load() {
		l.evalFailures++
	}
	if l.state == CLOSED {
		consecThresh := l.consecFailTrip
		// Use a more conservative threshold during warmup
		if l.samples < warmupSamples && consecThresh > warmupConsecTrip {
			consecThresh = warmupConsecTrip
		}
		sloErr := 1.0 - l.slo.SuccessRate
		if l.samples >= warmupSamples && l.errLowerBound() > sloErr {
			l.enterThrottled(ts, TriggerFailureRate)
		} else if l.consecFails >= consecThresh {
			l.enterThrottled(ts, TriggerConsecutiveFailures)
		}
	}
	return l.stateChange(previous)
}

func (l *Levee) updateErrEWMA(ts time.Time, sample float64) {
	if l.errLastTS.IsZero() {
		l.errEWMA, l.errLastTS = sample, ts
		return
	}
	dt := ts.Sub(l.errLastTS).Seconds()
	l.errEWMA += l.ewmaAlpha(dt, ewmaHalfLife) * (sample - l.errEWMA)
	l.errLastTS = ts
}

func (l *Levee) updateCapacityEstimate(ts time.Time, duration time.Duration) {
	prevTS := l.lastSuccessTS
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

	if l.surgeArmed {
		// Armed: the published onset is frozen so the queue-polluted estimates
		// cannot chase the surge; completions feed the proof of health instead.
		// The first completion anchors the proof-rate span (see surgeProvenCap).
		if l.evalSuccesses == 0 {
			l.lastEvalTS = ts
		}
		l.evalSuccesses++
	} else if !l.capped.Load() {
		if l.surgeProven > 0 && !prevTS.IsZero() {
			l.surgeProven *= math.Exp2(-ts.Sub(prevTS).Seconds() / goodputHalfLife.Seconds())
			if l.surgeProven < 0.5 {
				l.surgeProven = 0
			}
		}
		l.publishSurgeLimit()
	}
}

// surgeCheck runs when an uncapped admission finds inflight beyond the published
// onset. The request is already admitted; this only loads the spring: strain
// accumulates until proof of health relaxes it or the strain budget trips.
func (l *Levee) surgeCheck(ts time.Time) (StateChange, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previous := l.state

	if l.state != CLOSED || l.capped.Load() {
		return l.stateChange(previous), nil
	}
	lim := l.surgeLimit.Load()
	if lim == 0 || l.inflight.Load() <= lim {
		return StateChange{State: CLOSED}, nil
	}
	if !l.surgeArmed {
		// Arm: anchor latency before queueing pollutes it, and relax leftover
		// strain for the calm unarmed time since the spring was last active.
		l.surgeArmed, l.surgeLatSnap = true, l.avgLatency
		l.surgeStrain = max(l.surgeStrain-max(ts.Sub(l.surgeStrainTS).Seconds(), 0), 0)
		l.surgeStrainTS = ts
		l.resetEval(ts)
	}
	if l.surgeTick(ts) {
		l.enterSurgeThrottled(ts)
	}
	return l.stateChange(previous), nil
}

// surgeOnCompletion advances the armed spring on a completion; failures pass
// time without adding proof, successes may relax or rebase instead.
func (l *Levee) surgeOnCompletion(ts time.Time) {
	if l.surgeArmed && l.surgeTick(ts) {
		l.enterSurgeThrottled(ts)
	}
}

// surgeTick integrates the spring. Stretch is the fractional excess of inflight
// over the largest base the window's proof supports; positive stretch loads
// strain, proof or drain relaxes it, and a spent budget reports a trip. The
// stretch-proportional load is what makes urgency scale with spike size: time
// to trip is budget/stretch, with no fixed window or latency multiplier.
func (l *Levee) surgeTick(ts time.Time) (trip bool) {
	base := float64(l.surgeLimit.Load())
	if pc := l.surgeProvenCap(ts); pc > 0 {
		base = max(base, surgeOnset(pc))
	}
	stretch := float64(l.inflight.Load())/base - 1
	if dt := ts.Sub(l.surgeStrainTS).Seconds(); dt > 0 {
		l.surgeStrain = max(l.surgeStrain+stretch*dt, 0)
		l.surgeStrainTS = ts
	}
	if l.surgeStrain >= l.surgeStrainBudget() {
		return true
	}
	if stretch <= 0 && l.surgeStrain == 0 {
		l.surgeDisarm(ts)
	}
	return false
}

// surgeProvenCap is the concurrency this window's completions prove healthy:
// completion rate times the pre-surge latency (Little's law). Rate collapse and
// latency inflation both keep it low; absorbing the new regime raises it. The
// rate spans first counted completion to now, so k completions cover k-1 gaps.
func (l *Levee) surgeProvenCap(ts time.Time) float64 {
	if l.evalSuccesses < targetSamplesPerEval {
		return 0
	}
	elapsed := ts.Sub(l.lastEvalTS).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(l.evalSuccesses-1) / elapsed * l.surgeLatSnap
}

// surgeStrainBudget is the strain that trips: one eval interval at unit
// stretch, anchored to the pre-surge latency.
func (l *Levee) surgeStrainBudget() float64 {
	return evalIntervalFor(l.surgeLatSnap).Seconds()
}

// surgeDisarm ends an armed window whose stretch fully relaxed. Capacity
// proven beyond the current EWMA estimate is kept as a decaying excess so the
// published onset covers the new regime while the estimates catch up.
func (l *Levee) surgeDisarm(ts time.Time) {
	// Only raise: a short window proving less than an earlier proof must not
	// collapse the onset under a still-standing regime (decay handles shrink).
	if excess := l.surgeProvenCap(ts) - l.goodput*l.avgLatency; excess > l.surgeProven {
		l.surgeProven = excess
	}
	l.surgeArmed = false
	l.publishSurgeLimit()
}

// surgeOnset maps a capacity estimate to the inflight level where stretch
// begins: an upper bound on the healthyTailProb Poisson quantile, so healthy
// fluctuation essentially never loads the spring.
func surgeOnset(c float64) float64 {
	return c + surgeOnsetZ*(math.Sqrt(c)+surgeOnsetZ/4)
}

// publishSurgeLimit precomputes the lock-free stretch onset from the capacity
// estimate plus any decaying proven excess, disarmed until warmup establishes
// an estimate.
func (l *Levee) publishSurgeLimit() {
	if l.samples < warmupSamples || l.goodput <= 0 || l.avgLatency <= 0 {
		l.surgeLimit.Store(0)
		return
	}
	lim := surgeOnset(l.goodput*l.avgLatency + l.surgeProven)
	const maxSurgeLimit = float64(int64(1) << 40)
	if math.IsNaN(lim) || lim > maxSurgeLimit {
		lim = maxSurgeLimit
	}
	l.surgeLimit.Store(int64(math.Ceil(lim)))
}

func (l *Levee) throttleTo(ts time.Time, limit float64, trigger Trigger) {
	l.capped.Store(true)
	l.transition(THROTTLED, trigger, ts)
	l.inflightLimit = max(limit, minInflightLimit)
	l.consecFails = 0
	l.surgeArmed = false
	l.surgeLimit.Store(0) // stretch onset is meaningless while capped
	l.resetEval(ts)
}

// enterThrottled is the failure-evidence trip: seed at half the observed
// operating point (the inflight term protects cold/low-signal callers).
func (l *Levee) enterThrottled(ts time.Time, trigger Trigger) {
	capacity := l.goodput * l.avgLatency
	l.throttleTo(ts, max(capacity, float64(l.inflight.Load()))*0.5, trigger)
}

// enterSurgeThrottled is the proactive surge trip: the service is healthy and
// demand is the problem, so hold at the larger of the pre-surge capacity and
// the window's proven capacity -- never the ballooned inflight.
func (l *Levee) enterSurgeThrottled(ts time.Time) {
	seed := max(l.goodput*l.surgeLatSnap, l.surgeProvenCap(ts))
	budget := l.surgeStrainBudget()
	l.throttleTo(ts, seed, TriggerSurge)
	// The spring stays loaded through the trip: a re-stretch right after
	// recovery re-trips instantly; only calm uncapped time relaxes it.
	l.surgeStrain, l.surgeStrainTS = budget, ts
}

func (l *Levee) enterHalfOpen(ts time.Time) {
	l.transition(HALF_OPEN, TriggerCooldownExpired, ts)
	l.inflightLimit = minInflightLimit
	l.capped.Store(true)
	l.consecFails = 0
	l.surgeArmed = false
	l.surgeLimit.Store(0)
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
		l.enterThrottled(ts, TriggerFailureRate)
		return
	}
	if l.inflightLimit > float64(l.inflight.Load()+1)*3.0 {
		l.capped.Store(false)
		// The capped span served the standing inflight at a healthy error rate;
		// that is proof of health too, so the fresh onset must clear it. Without
		// this the lagging EWMAs re-breach on the next admission and the
		// trip-recover churn hides real demand from upstream autoscaling.
		if excess := float64(l.inflight.Load()) - l.goodput*l.avgLatency; excess > l.surgeProven {
			l.surgeProven = excess
		}
		l.surgeStrainTS = ts  // the capped span held the spring; relaxation resumes now
		l.publishSurgeLimit() // re-arm the fast path with a fresh anchor
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
			l.transition(OPEN, TriggerMinLimitFailureRate, ts)
			l.consecFails = 0
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

func evalIntervalFor(latency float64) time.Duration {
	if latency <= 0 {
		return minEvalInterval
	}
	adaptive := time.Duration(latency * float64(targetSamplesPerEval) * float64(time.Second))
	return min(max(adaptive, minEvalInterval), maxEvalInterval)
}

func (l *Levee) baseEvalInterval() time.Duration {
	return evalIntervalFor(l.avgLatency)
}

func (l *Levee) effectiveRecoveryHoldoff() time.Duration {
	return max(ewmaHalfLife, l.baseEvalInterval()*2)
}

// Call wraps a function: it admits, runs f (timed by the wall clock), and reports
// the outcome. Rejected admission returns [ErrCircuitOpen] without running f.
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
