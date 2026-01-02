package levee

import "time"

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
// Returns nil if EWMAs haven't been initialized yet
func (l *Levee) SaveState() (*LeveeState, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Check if EWMAs have been initialized (if one is nil, all are nil)
	if l.metrics.concurrency.value == nil {
		return nil, nil
	}

	state := &LeveeState{
		SLO:        l.slo,
		BufferSize: l.metrics.concurrency._size,

		// Concurrency EWMAs
		ConcurrencyValueBase:     l.metrics.concurrency.value.base,
		ConcurrencyValueMid:      l.metrics.concurrency.value.ewmaMid,
		ConcurrencyValueLong:     l.metrics.concurrency.value.ewmaLong,
		ConcurrencyDeviationBase: l.metrics.concurrency.deviation.base,
		ConcurrencyDeviationMid:  l.metrics.concurrency.deviation.ewmaMid,
		ConcurrencyDeviationLong: l.metrics.concurrency.deviation.ewmaLong,

		// Latency EWMAs
		LatencyValueBase:     l.metrics.latency.value.base,
		LatencyValueMid:      l.metrics.latency.value.ewmaMid,
		LatencyValueLong:     l.metrics.latency.value.ewmaLong,
		LatencyDeviationBase: l.metrics.latency.deviation.base,
		LatencyDeviationMid:  l.metrics.latency.deviation.ewmaMid,
		LatencyDeviationLong: l.metrics.latency.deviation.ewmaLong,

		// Error EWMAs
		ErrorsValueBase:     l.metrics.errors.value.base,
		ErrorsValueMid:      l.metrics.errors.value.ewmaMid,
		ErrorsValueLong:     l.metrics.errors.value.ewmaLong,
		ErrorsDeviationBase: l.metrics.errors.deviation.base,
		ErrorsDeviationMid:  l.metrics.errors.deviation.ewmaMid,
		ErrorsDeviationLong: l.metrics.errors.deviation.ewmaLong,
	}

	return state, nil
}

// RestoreState creates a new Levee in CLOSED state with EWMAs restored from saved state
func RestoreState(state *LeveeState) *Levee {
	l := &Levee{
		slo:     state.SLO,
		metrics: *newMetrics(state.BufferSize),
		state:   CLOSED,
	}
	l.lastOpenAt.Store(time.Time{})

	// Restore concurrency EWMAs
	l.metrics.concurrency.value = &EWMA{
		base:     state.ConcurrencyValueBase,
		ewmaMid:  state.ConcurrencyValueMid,
		ewmaLong: state.ConcurrencyValueLong,
	}
	l.metrics.concurrency.deviation = &EWMA{
		base:     state.ConcurrencyDeviationBase,
		ewmaMid:  state.ConcurrencyDeviationMid,
		ewmaLong: state.ConcurrencyDeviationLong,
	}

	// Restore latency EWMAs
	l.metrics.latency.value = &EWMA{
		base:     state.LatencyValueBase,
		ewmaMid:  state.LatencyValueMid,
		ewmaLong: state.LatencyValueLong,
	}
	l.metrics.latency.deviation = &EWMA{
		base:     state.LatencyDeviationBase,
		ewmaMid:  state.LatencyDeviationMid,
		ewmaLong: state.LatencyDeviationLong,
	}

	// Restore error EWMAs
	l.metrics.errors.value = &EWMA{
		base:     state.ErrorsValueBase,
		ewmaMid:  state.ErrorsValueMid,
		ewmaLong: state.ErrorsValueLong,
	}
	l.metrics.errors.deviation = &EWMA{
		base:     state.ErrorsDeviationBase,
		ewmaMid:  state.ErrorsDeviationMid,
		ewmaLong: state.ErrorsDeviationLong,
	}

	return l
}
