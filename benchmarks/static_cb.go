package benchmarks

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee"
)

// staticState is the internal state type for StaticCB
// It maintains the traditional 3-state model (CLOSED, OPEN, HALF_OPEN)
type staticState uint8

const (
	staticClosed staticState = iota
	staticOpen
	staticHalfOpen
)

// StaticCBConfig holds the configuration for a static circuit breaker
type StaticCBConfig struct {
	FailureThreshold int           // Consecutive failures to open circuit
	SuccessThreshold int           // Consecutive successes in HALF_OPEN to close circuit
	HalfOpenTimeout  time.Duration // Time in OPEN before transitioning to HALF_OPEN
	HalfOpenMaxCalls int           // Max concurrent calls allowed in HALF_OPEN (0 = unlimited)
}

// StaticCB is a conventional threshold-based circuit breaker
// It implements the CircuitBreaker interface for benchmark comparison
type StaticCB struct {
	mu     sync.RWMutex
	config StaticCBConfig

	internalState        staticState // Internal 3-state tracking
	consecutiveFailures  int
	consecutiveSuccesses int
	lastOpenAt           atomic.Value // time.Time
	halfOpenCalls        int32        // atomic counter for HALF_OPEN calls
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
		config:        config,
		internalState: staticClosed,
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

// toExternalState converts internal state to levee.State (must hold lock)
func (cb *StaticCB) toExternalState() levee.State {
	switch cb.internalState {
	case staticClosed:
		return levee.CLOSED
	default: // staticOpen, staticHalfOpen
		return levee.OPEN
	}
}

// State returns the current state of the circuit breaker
// Maps internal HALF_OPEN to levee.OPEN for external representation
func (cb *StaticCB) State() levee.State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.toExternalState()
}

// StateUpdates returns nil (not implemented for benchmarks)
func (cb *StaticCB) StateUpdates() <-chan levee.StateChange {
	return nil
}

// Start performs pre-call checks and returns the current state
func (cb *StaticCB) Start(ts time.Time) (levee.StateChange, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.internalState {
	case staticOpen:
		// Check if timeout has elapsed
		lastOpen := cb.lastOpenAt.Load().(time.Time)
		if ts.Sub(lastOpen) >= cb.config.HalfOpenTimeout {
			// Transition to half-open for probing
			cb.internalState = staticHalfOpen
			cb.consecutiveSuccesses = 0
			atomic.StoreInt32(&cb.halfOpenCalls, 0)
		} else {
			return levee.StateChange{State: levee.OPEN}, levee.ErrCircuitOpen
		}
		fallthrough

	case staticHalfOpen:
		// Check if we've exceeded max concurrent calls in half-open
		if cb.config.HalfOpenMaxCalls > 0 {
			currentCalls := atomic.AddInt32(&cb.halfOpenCalls, 1)
			if int(currentCalls) > cb.config.HalfOpenMaxCalls {
				atomic.AddInt32(&cb.halfOpenCalls, -1)
				return levee.StateChange{State: levee.OPEN}, levee.ErrCircuitOpen
			}
		}
		return levee.StateChange{State: levee.OPEN}, nil // HALF_OPEN maps to OPEN

	case staticClosed:
		return levee.StateChange{State: levee.CLOSED}, nil

	default:
		cb.internalState = staticClosed
		return levee.StateChange{State: levee.CLOSED}, nil
	}
}

// Success processes a successful call result
func (cb *StaticCB) Success(ts time.Time, duration time.Duration) levee.StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Decrement half-open counter if applicable
	if cb.internalState == staticHalfOpen {
		atomic.AddInt32(&cb.halfOpenCalls, -1)
	}

	// Reset consecutive failures on success
	cb.consecutiveFailures = 0
	cb.consecutiveSuccesses++

	// Check if we should close the circuit (from half-open)
	if cb.internalState == staticHalfOpen && cb.consecutiveSuccesses >= cb.config.SuccessThreshold {
		cb.internalState = staticClosed
		cb.consecutiveSuccesses = 0
	}

	return levee.StateChange{State: cb.toExternalState()}
}

// Fail processes a failed call result
func (cb *StaticCB) Fail(ts time.Time, duration time.Duration) levee.StateChange {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Decrement half-open counter if applicable
	if cb.internalState == staticHalfOpen {
		atomic.AddInt32(&cb.halfOpenCalls, -1)
	}

	// Reset consecutive successes on failure
	cb.consecutiveSuccesses = 0
	cb.consecutiveFailures++

	// Check if we should open the circuit
	switch cb.internalState {
	case staticClosed:
		if cb.consecutiveFailures >= cb.config.FailureThreshold {
			cb.internalState = staticOpen
			cb.lastOpenAt.Store(ts)
			cb.consecutiveFailures = 0
		}
	case staticHalfOpen:
		// Any failure in half-open reopens the circuit
		cb.internalState = staticOpen
		cb.lastOpenAt.Store(ts)
		cb.consecutiveFailures = 0
	}

	return levee.StateChange{State: cb.toExternalState()}
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

// StaticCBState holds the serializable state of a StaticCB for checkpointing.
type StaticCBState struct {
	InternalState        uint8 `json:"internal_state"`
	ConsecutiveFailures  int   `json:"consecutive_failures"`
	ConsecutiveSuccesses int   `json:"consecutive_successes"`
	LastOpenAtNS         int64 `json:"last_open_at_ns"`
	HalfOpenCalls        int32 `json:"half_open_calls"`
}

// SaveState serializes the current state for checkpointing.
func (cb *StaticCB) SaveState() *StaticCBState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	lastOpen := cb.lastOpenAt.Load().(time.Time)
	var lastOpenNS int64
	if !lastOpen.IsZero() {
		lastOpenNS = lastOpen.UnixNano()
	}

	return &StaticCBState{
		InternalState:        uint8(cb.internalState),
		ConsecutiveFailures:  cb.consecutiveFailures,
		ConsecutiveSuccesses: cb.consecutiveSuccesses,
		LastOpenAtNS:         lastOpenNS,
		HalfOpenCalls:        atomic.LoadInt32(&cb.halfOpenCalls),
	}
}

// RestoreStaticCB creates a StaticCB from a saved state.
func RestoreStaticCB(config StaticCBConfig, state *StaticCBState) *StaticCB {
	cb := &StaticCB{
		config:               config,
		internalState:        staticState(state.InternalState),
		consecutiveFailures:  state.ConsecutiveFailures,
		consecutiveSuccesses: state.ConsecutiveSuccesses,
	}

	var lastOpen time.Time
	if state.LastOpenAtNS != 0 {
		lastOpen = time.Unix(0, state.LastOpenAtNS)
	}
	cb.lastOpenAt.Store(lastOpen)
	atomic.StoreInt32(&cb.halfOpenCalls, state.HalfOpenCalls)

	return cb
}

// NoCB is a "circuit breaker" that never blocks - used as baseline
type NoCB struct{}

// NewNoCB creates a new NoCB instance
func NewNoCB() *NoCB {
	return &NoCB{}
}

// Start always allows requests (returns CLOSED state, no error)
func (cb *NoCB) Start(ts time.Time) (levee.StateChange, error) {
	return levee.StateChange{State: levee.CLOSED}, nil
}

// Success is a no-op that returns CLOSED state
func (cb *NoCB) Success(ts time.Time, duration time.Duration) levee.StateChange {
	return levee.StateChange{State: levee.CLOSED}
}

// Fail is a no-op that returns CLOSED state
func (cb *NoCB) Fail(ts time.Time, duration time.Duration) levee.StateChange {
	return levee.StateChange{State: levee.CLOSED}
}

// State always returns CLOSED
func (cb *NoCB) State() levee.State {
	return levee.CLOSED
}
