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
	ConcurrencyP99Base       float64
	ConcurrencyP99Mid        float64
	ConcurrencyP99Long       float64
	ConcurrencyDeviationBase float64
	ConcurrencyDeviationMid  float64
	ConcurrencyDeviationLong float64
	ConcurrencyDerivBase     float64
	ConcurrencyDerivMid      float64
	ConcurrencyDerivLong     float64

	// Latency EWMAs
	LatencyValueBase     float64
	LatencyValueMid      float64
	LatencyValueLong     float64
	LatencyP99Base       float64
	LatencyP99Mid        float64
	LatencyP99Long       float64
	LatencyDeviationBase float64
	LatencyDeviationMid  float64
	LatencyDeviationLong float64
	LatencyDerivBase     float64
	LatencyDerivMid      float64
	LatencyDerivLong     float64

	// Error rate EWMAs
	ErrorsValueBase     float64
	ErrorsValueMid      float64
	ErrorsValueLong     float64
	ErrorsP99Base       float64
	ErrorsP99Mid        float64
	ErrorsP99Long       float64
	ErrorsDeviationBase float64
	ErrorsDeviationMid  float64
	ErrorsDeviationLong float64
	ErrorsDerivBase     float64
	ErrorsDerivMid      float64
	ErrorsDerivLong     float64

	// Request count EWMAs
	RequestsValueBase     float64
	RequestsValueMid      float64
	RequestsValueLong     float64
	RequestsP99Base       float64
	RequestsP99Mid        float64
	RequestsP99Long       float64
	RequestsDeviationBase float64
	RequestsDeviationMid  float64
	RequestsDeviationLong float64
	RequestsDerivBase     float64
	RequestsDerivMid      float64
	RequestsDerivLong     float64
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
		ConcurrencyP99Base:       cb.metrics.concurrency.p99.base,
		ConcurrencyP99Mid:        cb.metrics.concurrency.p99.ewmaMid,
		ConcurrencyP99Long:       cb.metrics.concurrency.p99.ewmaLong,
		ConcurrencyDeviationBase: cb.metrics.concurrency.deviation.base,
		ConcurrencyDeviationMid:  cb.metrics.concurrency.deviation.ewmaMid,
		ConcurrencyDeviationLong: cb.metrics.concurrency.deviation.ewmaLong,
		ConcurrencyDerivBase:     cb.metrics.concurrency.derivative.base,
		ConcurrencyDerivMid:      cb.metrics.concurrency.derivative.ewmaMid,
		ConcurrencyDerivLong:     cb.metrics.concurrency.derivative.ewmaLong,

		// Latency EWMAs
		LatencyValueBase:     cb.metrics.latency.value.base,
		LatencyValueMid:      cb.metrics.latency.value.ewmaMid,
		LatencyValueLong:     cb.metrics.latency.value.ewmaLong,
		LatencyP99Base:       cb.metrics.latency.p99.base,
		LatencyP99Mid:        cb.metrics.latency.p99.ewmaMid,
		LatencyP99Long:       cb.metrics.latency.p99.ewmaLong,
		LatencyDeviationBase: cb.metrics.latency.deviation.base,
		LatencyDeviationMid:  cb.metrics.latency.deviation.ewmaMid,
		LatencyDeviationLong: cb.metrics.latency.deviation.ewmaLong,
		LatencyDerivBase:     cb.metrics.latency.derivative.base,
		LatencyDerivMid:      cb.metrics.latency.derivative.ewmaMid,
		LatencyDerivLong:     cb.metrics.latency.derivative.ewmaLong,

		// Error EWMAs
		ErrorsValueBase:     cb.metrics.errors.value.base,
		ErrorsValueMid:      cb.metrics.errors.value.ewmaMid,
		ErrorsValueLong:     cb.metrics.errors.value.ewmaLong,
		ErrorsP99Base:       cb.metrics.errors.p99.base,
		ErrorsP99Mid:        cb.metrics.errors.p99.ewmaMid,
		ErrorsP99Long:       cb.metrics.errors.p99.ewmaLong,
		ErrorsDeviationBase: cb.metrics.errors.deviation.base,
		ErrorsDeviationMid:  cb.metrics.errors.deviation.ewmaMid,
		ErrorsDeviationLong: cb.metrics.errors.deviation.ewmaLong,
		ErrorsDerivBase:     cb.metrics.errors.derivative.base,
		ErrorsDerivMid:      cb.metrics.errors.derivative.ewmaMid,
		ErrorsDerivLong:     cb.metrics.errors.derivative.ewmaLong,

		// Request EWMAs
		RequestsValueBase:     cb.metrics.requests.value.base,
		RequestsValueMid:      cb.metrics.requests.value.ewmaMid,
		RequestsValueLong:     cb.metrics.requests.value.ewmaLong,
		RequestsP99Base:       cb.metrics.requests.p99.base,
		RequestsP99Mid:        cb.metrics.requests.p99.ewmaMid,
		RequestsP99Long:       cb.metrics.requests.p99.ewmaLong,
		RequestsDeviationBase: cb.metrics.requests.deviation.base,
		RequestsDeviationMid:  cb.metrics.requests.deviation.ewmaMid,
		RequestsDeviationLong: cb.metrics.requests.deviation.ewmaLong,
		RequestsDerivBase:     cb.metrics.requests.derivative.base,
		RequestsDerivMid:      cb.metrics.requests.derivative.ewmaMid,
		RequestsDerivLong:     cb.metrics.requests.derivative.ewmaLong,
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
	cb.metrics.concurrency.p99 = &EWMA{
		base:     state.ConcurrencyP99Base,
		ewmaMid:  state.ConcurrencyP99Mid,
		ewmaLong: state.ConcurrencyP99Long,
	}
	cb.metrics.concurrency.deviation = &EWMA{
		base:     state.ConcurrencyDeviationBase,
		ewmaMid:  state.ConcurrencyDeviationMid,
		ewmaLong: state.ConcurrencyDeviationLong,
	}
	cb.metrics.concurrency.derivative = &EWMA{
		base:     state.ConcurrencyDerivBase,
		ewmaMid:  state.ConcurrencyDerivMid,
		ewmaLong: state.ConcurrencyDerivLong,
	}

	// Restore latency EWMAs
	cb.metrics.latency.value = &EWMA{
		base:     state.LatencyValueBase,
		ewmaMid:  state.LatencyValueMid,
		ewmaLong: state.LatencyValueLong,
	}
	cb.metrics.latency.p99 = &EWMA{
		base:     state.LatencyP99Base,
		ewmaMid:  state.LatencyP99Mid,
		ewmaLong: state.LatencyP99Long,
	}
	cb.metrics.latency.deviation = &EWMA{
		base:     state.LatencyDeviationBase,
		ewmaMid:  state.LatencyDeviationMid,
		ewmaLong: state.LatencyDeviationLong,
	}
	cb.metrics.latency.derivative = &EWMA{
		base:     state.LatencyDerivBase,
		ewmaMid:  state.LatencyDerivMid,
		ewmaLong: state.LatencyDerivLong,
	}

	// Restore error EWMAs
	cb.metrics.errors.value = &EWMA{
		base:     state.ErrorsValueBase,
		ewmaMid:  state.ErrorsValueMid,
		ewmaLong: state.ErrorsValueLong,
	}
	cb.metrics.errors.p99 = &EWMA{
		base:     state.ErrorsP99Base,
		ewmaMid:  state.ErrorsP99Mid,
		ewmaLong: state.ErrorsP99Long,
	}
	cb.metrics.errors.deviation = &EWMA{
		base:     state.ErrorsDeviationBase,
		ewmaMid:  state.ErrorsDeviationMid,
		ewmaLong: state.ErrorsDeviationLong,
	}
	cb.metrics.errors.derivative = &EWMA{
		base:     state.ErrorsDerivBase,
		ewmaMid:  state.ErrorsDerivMid,
		ewmaLong: state.ErrorsDerivLong,
	}

	// Restore request EWMAs
	cb.metrics.requests.value = &EWMA{
		base:     state.RequestsValueBase,
		ewmaMid:  state.RequestsValueMid,
		ewmaLong: state.RequestsValueLong,
	}
	cb.metrics.requests.p99 = &EWMA{
		base:     state.RequestsP99Base,
		ewmaMid:  state.RequestsP99Mid,
		ewmaLong: state.RequestsP99Long,
	}
	cb.metrics.requests.deviation = &EWMA{
		base:     state.RequestsDeviationBase,
		ewmaMid:  state.RequestsDeviationMid,
		ewmaLong: state.RequestsDeviationLong,
	}
	cb.metrics.requests.derivative = &EWMA{
		base:     state.RequestsDerivBase,
		ewmaMid:  state.RequestsDerivMid,
		ewmaLong: state.RequestsDerivLong,
	}

	// Create Levee with restored CircuitBreaker, already in ready state (CLOSED)
	return &Levee{
		ready: true,
		cb:    cb,
		state: make(chan State),
	}
}
