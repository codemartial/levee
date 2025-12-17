package levee

// LeveeState represents the saved EWMA state from a Levee circuit breaker
// This struct is designed to be serializable (e.g., with JSON/gob encoding)
type LeveeState struct {
	SLO        SLO
	BufferSize uint16

	// Concurrency EWMAs
	ConcurrencyValueBase     float64
	ConcurrencyValueMid      float64
	ConcurrencyValueLong     float64
	ConcurrencyDeviationBase float64
	ConcurrencyDeviationMid  float64
	ConcurrencyDeviationLong float64

	// Latency EWMAs
	LatencyValueBase     float64
	LatencyValueMid      float64
	LatencyValueLong     float64
	LatencyDeviationBase float64
	LatencyDeviationMid  float64
	LatencyDeviationLong float64

	// Error rate EWMAs
	ErrorsValueBase     float64
	ErrorsValueMid      float64
	ErrorsValueLong     float64
	ErrorsDeviationBase float64
	ErrorsDeviationMid  float64
	ErrorsDeviationLong float64
}

// SaveState extracts the EWMA state from a Levee instance
// Returns nil if Levee is not ready or if EWMAs haven't been initialized yet
func (l *Levee) SaveState() (*LeveeState, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Only save state if Levee is ready (using CircuitBreaker, not WarmupCB)
	if !l.ready {
		return nil, nil
	}

	// Type assert to CircuitBreaker to access metrics
	cb, ok := l.cb.(*CircuitBreaker)
	if !ok {
		return nil, nil
	}

	cb.mu.RLock()
	defer cb.mu.RUnlock()

	// Check if EWMAs have been initialized (if one is nil, all are nil)
	if cb.metrics.concurrency.value == nil {
		return nil, nil
	}

	state := &LeveeState{
		SLO:        cb.stated_slo,
		BufferSize: cb.metrics.concurrency._size,

		// Concurrency EWMAs
		ConcurrencyValueBase:     cb.metrics.concurrency.value.base,
		ConcurrencyValueMid:      cb.metrics.concurrency.value.ewmaMid,
		ConcurrencyValueLong:     cb.metrics.concurrency.value.ewmaLong,
		ConcurrencyDeviationBase: cb.metrics.concurrency.deviation.base,
		ConcurrencyDeviationMid:  cb.metrics.concurrency.deviation.ewmaMid,
		ConcurrencyDeviationLong: cb.metrics.concurrency.deviation.ewmaLong,

		// Latency EWMAs
		LatencyValueBase:     cb.metrics.latency.value.base,
		LatencyValueMid:      cb.metrics.latency.value.ewmaMid,
		LatencyValueLong:     cb.metrics.latency.value.ewmaLong,
		LatencyDeviationBase: cb.metrics.latency.deviation.base,
		LatencyDeviationMid:  cb.metrics.latency.deviation.ewmaMid,
		LatencyDeviationLong: cb.metrics.latency.deviation.ewmaLong,

		// Error EWMAs
		ErrorsValueBase:     cb.metrics.errors.value.base,
		ErrorsValueMid:      cb.metrics.errors.value.ewmaMid,
		ErrorsValueLong:     cb.metrics.errors.value.ewmaLong,
		ErrorsDeviationBase: cb.metrics.errors.deviation.base,
		ErrorsDeviationMid:  cb.metrics.errors.deviation.ewmaMid,
		ErrorsDeviationLong: cb.metrics.errors.deviation.ewmaLong,
	}

	return state, nil
}

// RestoreState creates a new Levee in CLOSED state with EWMAs restored from saved state
func RestoreState(state *LeveeState) *Levee {
	// Create a new CircuitBreaker with the saved SLO and buffer size
	cb := NewCircuitBreaker(state.SLO, state.BufferSize)

	// Restore concurrency EWMAs
	cb.metrics.concurrency.value = &EWMA{
		base:     state.ConcurrencyValueBase,
		ewmaMid:  state.ConcurrencyValueMid,
		ewmaLong: state.ConcurrencyValueLong,
	}
	cb.metrics.concurrency.deviation = &EWMA{
		base:     state.ConcurrencyDeviationBase,
		ewmaMid:  state.ConcurrencyDeviationMid,
		ewmaLong: state.ConcurrencyDeviationLong,
	}

	// Restore latency EWMAs
	cb.metrics.latency.value = &EWMA{
		base:     state.LatencyValueBase,
		ewmaMid:  state.LatencyValueMid,
		ewmaLong: state.LatencyValueLong,
	}
	cb.metrics.latency.deviation = &EWMA{
		base:     state.LatencyDeviationBase,
		ewmaMid:  state.LatencyDeviationMid,
		ewmaLong: state.LatencyDeviationLong,
	}

	// Restore error EWMAs
	cb.metrics.errors.value = &EWMA{
		base:     state.ErrorsValueBase,
		ewmaMid:  state.ErrorsValueMid,
		ewmaLong: state.ErrorsValueLong,
	}
	cb.metrics.errors.deviation = &EWMA{
		base:     state.ErrorsDeviationBase,
		ewmaMid:  state.ErrorsDeviationMid,
		ewmaLong: state.ErrorsDeviationLong,
	}

	// Create Levee with restored CircuitBreaker, already in ready state (CLOSED)
	return &Levee{
		ready: true,
		cb:    cb,
		state: make(chan State),
	}
}
