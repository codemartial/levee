package benchmarks

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee"
)

// StaticCBConfig holds the configuration for a static circuit breaker
type StaticCBConfig struct {
	FailureThreshold int           // Consecutive failures to open circuit
	SuccessThreshold int           // Consecutive successes in HALF_OPEN to close circuit
	HalfOpenTimeout  time.Duration // Time in OPEN before transitioning to HALF_OPEN
	HalfOpenMaxCalls int           // Max concurrent calls allowed in HALF_OPEN (0 = unlimited)
}

// StaticCB is a conventional threshold-based circuit breaker
// It implements levee.ICircuitBreaker for benchmark comparison
type StaticCB struct {
	mu     sync.RWMutex
	config StaticCBConfig

	state              levee.State
	consecutiveFailures int
	consecutiveSuccesses int
	lastOpenAt         atomic.Value // time.Time
	halfOpenCalls      int32        // atomic counter for HALF_OPEN calls
}

// Predefined configurations for benchmarking
//
// Derivation assumptions (no foreknowledge of actual test traffic):
//   - SLO: 90% success rate (10% error threshold)
//   - Expected latencies: P50 ~ 50ms, P99 ~ 150ms
//
// For consecutive-failure CB, expected requests until T consecutive failures at error rate p:
//   E[n] ≈ 1 / (p^T × (1-p))
//
// Goal: Don't trip at SLO boundary (10%), trip quickly at ≥15-20% errors
var (
	// StaticBAUConfig - designed for BAU traffic range: 100-800 RPS
	//
	// FailureThreshold derivation at max 800 RPS:
	//   - At 10% errors, T=5: E[n] ≈ 111,111 req → 139 sec (won't trip at SLO boundary)
	//   - At 15% errors, T=5: E[n] ≈ 15,504 req  → 19 sec  (trips reasonably fast)
	//   - At 20% errors, T=5: E[n] ≈ 3,906 req   → 4.9 sec (trips quickly)
	//
	// SuccessThreshold: P(3 consecutive at 90%) = 0.9³ = 72.9% - quick recovery
	// HalfOpenTimeout:  10s = ~67 P99 cycles - sufficient for low-traffic recovery
	// HalfOpenMaxCalls: 1 - conservative probing at lower traffic
	StaticBAUConfig = StaticCBConfig{
		FailureThreshold: 5,
		SuccessThreshold: 3,
		HalfOpenTimeout:  10 * time.Second,
		HalfOpenMaxCalls: 1,
	}

	// StaticPeakConfig - designed for Peak traffic range: 2000-5000 RPS
	//
	// FailureThreshold derivation at max 5000 RPS:
	//   - At 10% errors, T=5: E[n] ≈ 111,111 req → 22 sec (would trip at SLO boundary!)
	//   - Need higher threshold to account for volume. With T=7:
	//   - At 10% errors, T=7: E[n] ≈ 11,111,111 req → 2222 sec (won't trip)
	//   - At 15% errors, T=7: E[n] ≈ 688,456 req    → 138 sec  (trips eventually)
	//   - At 20% errors, T=7: E[n] ≈ 97,656 req     → 19.5 sec (trips reasonably)
	//
	// SuccessThreshold: P(5 consecutive at 90%) = 0.9⁵ = 59% - more validation at high traffic
	// HalfOpenTimeout:  30s = ~200 P99 cycles - more stabilization time at high traffic
	// HalfOpenMaxCalls: 3 - faster validation without overwhelming recovering backend
	StaticPeakConfig = StaticCBConfig{
		FailureThreshold: 7,
		SuccessThreshold: 5,
		HalfOpenTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 3,
	}
)

// NewStaticCB creates a new static circuit breaker with the given configuration
func NewStaticCB(config StaticCBConfig) *StaticCB {
	cb := &StaticCB{
		config: config,
		state:  levee.CLOSED,
	}
	cb.lastOpenAt.Store(time.Time{})
	return cb
}

// NewStaticBAU creates a StaticCB with BAU-optimized configuration
func NewStaticBAU() *StaticCB {
	return NewStaticCB(StaticBAUConfig)
}

// NewStaticPeak creates a StaticCB with Peak-optimized configuration
func NewStaticPeak() *StaticCB {
	return NewStaticCB(StaticPeakConfig)
}

// State returns the current state of the circuit breaker
func (cb *StaticCB) State() levee.State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// StateUpdates returns nil (not implemented for benchmarks)
func (cb *StaticCB) StateUpdates() <-chan levee.StateChange {
	return nil
}

// Start performs pre-call checks and returns the current state
func (cb *StaticCB) Start(ts time.Time) (levee.StateChange, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case levee.OPEN:
		// Check if timeout has elapsed
		lastOpen := cb.lastOpenAt.Load().(time.Time)
		if ts.Sub(lastOpen) >= cb.config.HalfOpenTimeout {
			// Transition to HALF_OPEN
			cb.state = levee.HALF_OPEN
			cb.consecutiveSuccesses = 0
			atomic.StoreInt32(&cb.halfOpenCalls, 0)
		} else {
			return levee.StateChange{State: levee.OPEN}, levee.ErrCircuitOpen
		}
		fallthrough

	case levee.HALF_OPEN:
		// Check if we've exceeded max concurrent calls in HALF_OPEN
		if cb.config.HalfOpenMaxCalls > 0 {
			currentCalls := atomic.AddInt32(&cb.halfOpenCalls, 1)
			if int(currentCalls) > cb.config.HalfOpenMaxCalls {
				atomic.AddInt32(&cb.halfOpenCalls, -1)
				return levee.StateChange{State: levee.HALF_OPEN}, levee.ErrCircuitHalfOpen
			}
		}
		return levee.StateChange{State: levee.HALF_OPEN}, nil

	case levee.CLOSED:
		return levee.StateChange{State: levee.CLOSED}, nil

	default:
		// INIT state - treat as CLOSED
		cb.state = levee.CLOSED
		return levee.StateChange{State: levee.CLOSED}, nil
	}
}

// Success processes a successful call result
func (cb *StaticCB) Success(ts time.Time, duration time.Duration) levee.StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Decrement HALF_OPEN counter if applicable
	if cb.state == levee.HALF_OPEN {
		atomic.AddInt32(&cb.halfOpenCalls, -1)
	}

	// Reset consecutive failures on success
	cb.consecutiveFailures = 0
	cb.consecutiveSuccesses++

	// Check if we should close the circuit (from HALF_OPEN)
	if cb.state == levee.HALF_OPEN && cb.consecutiveSuccesses >= cb.config.SuccessThreshold {
		cb.state = levee.CLOSED
		cb.consecutiveSuccesses = 0
	}

	return levee.StateChange{State: cb.state}
}

// Fail processes a failed call result
func (cb *StaticCB) Fail(ts time.Time, duration time.Duration) levee.StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Decrement HALF_OPEN counter if applicable
	if cb.state == levee.HALF_OPEN {
		atomic.AddInt32(&cb.halfOpenCalls, -1)
	}

	// Reset consecutive successes on failure
	cb.consecutiveSuccesses = 0
	cb.consecutiveFailures++

	// Check if we should open the circuit
	switch cb.state {
	case levee.CLOSED:
		if cb.consecutiveFailures >= cb.config.FailureThreshold {
			cb.state = levee.OPEN
			cb.lastOpenAt.Store(ts)
			cb.consecutiveFailures = 0
		}
	case levee.HALF_OPEN:
		// Any failure in HALF_OPEN reopens the circuit
		cb.state = levee.OPEN
		cb.lastOpenAt.Store(ts)
		cb.consecutiveFailures = 0
	}

	return levee.StateChange{State: cb.state}
}

// Call executes the given function with circuit breaker protection
func (cb *StaticCB) Call(f func() error) (levee.StateChange, error) {
	start := time.Now()

	sc, err := cb.Start(start)
	if err != nil {
		return sc, err
	}

	callErr := f()
	end := time.Now()
	duration := end.Sub(start)

	var resultState levee.StateChange
	if callErr != nil {
		resultState = cb.Fail(end, duration)
	} else {
		resultState = cb.Success(end, duration)
	}

	return resultState, callErr
}
