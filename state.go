package levee

import "time"

// LeveeState represents the saved EWMA state from a Levee circuit breaker
// This struct is designed to be serializable (e.g., with JSON/gob encoding)
type LeveeState struct {
	SLO        SLO
	BufferSize uint16

	// Successes EWMAs
	SuccessesValueBase     float64
	SuccessesValueMid      float64
	SuccessesValueLong     float64
	SuccessesDeviationBase float64
	SuccessesDeviationMid  float64
	SuccessesDeviationLong float64
	SuccessesTMeanBase     float64
	SuccessesTMeanMid      float64
	SuccessesTMeanLong     float64

	// Latency EWMAs
	LatencyValueBase     float64
	LatencyValueMid      float64
	LatencyValueLong     float64
	LatencyDeviationBase float64
	LatencyDeviationMid  float64
	LatencyDeviationLong float64
	LatencyTMeanBase     float64
	LatencyTMeanMid      float64
	LatencyTMeanLong     float64
}

// SaveState extracts the EWMA state from a Levee instance
// Returns nil if EWMAs haven't been initialized yet
func (l *Levee) SaveState() (*LeveeState, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Check if EWMAs have been initialized
	if l.metrics.successes.value == nil {
		return nil, nil
	}

	state := &LeveeState{
		SLO:        l.slo,
		BufferSize: l.metrics.successes._size,

		// Successes EWMAs
		SuccessesValueBase:     l.metrics.successes.value.base,
		SuccessesValueMid:      l.metrics.successes.value.ewmaMid,
		SuccessesValueLong:     l.metrics.successes.value.ewmaLong,
		SuccessesDeviationBase: l.metrics.successes.deviation.base,
		SuccessesDeviationMid:  l.metrics.successes.deviation.ewmaMid,
		SuccessesDeviationLong: l.metrics.successes.deviation.ewmaLong,

		// Latency EWMAs
		LatencyValueBase:     l.metrics.latency.value.base,
		LatencyValueMid:      l.metrics.latency.value.ewmaMid,
		LatencyValueLong:     l.metrics.latency.value.ewmaLong,
		LatencyDeviationBase: l.metrics.latency.deviation.base,
		LatencyDeviationMid:  l.metrics.latency.deviation.ewmaMid,
		LatencyDeviationLong: l.metrics.latency.deviation.ewmaLong,
	}

	// TMean may not be initialized yet (requires two buffer wraps)
	if l.metrics.successes.tMean != nil {
		state.SuccessesTMeanBase = l.metrics.successes.tMean.base
		state.SuccessesTMeanMid = l.metrics.successes.tMean.ewmaMid
		state.SuccessesTMeanLong = l.metrics.successes.tMean.ewmaLong
	}
	if l.metrics.latency.tMean != nil {
		state.LatencyTMeanBase = l.metrics.latency.tMean.base
		state.LatencyTMeanMid = l.metrics.latency.tMean.ewmaMid
		state.LatencyTMeanLong = l.metrics.latency.tMean.ewmaLong
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

	// Restore successes EWMAs
	l.metrics.successes.value = &EWMA{
		base:     state.SuccessesValueBase,
		ewmaMid:  state.SuccessesValueMid,
		ewmaLong: state.SuccessesValueLong,
	}
	l.metrics.successes.deviation = &EWMA{
		base:     state.SuccessesDeviationBase,
		ewmaMid:  state.SuccessesDeviationMid,
		ewmaLong: state.SuccessesDeviationLong,
	}
	if state.SuccessesTMeanBase != 0 || state.SuccessesTMeanMid != 0 || state.SuccessesTMeanLong != 0 {
		l.metrics.successes.tMean = &EWMA{
			base:     state.SuccessesTMeanBase,
			ewmaMid:  state.SuccessesTMeanMid,
			ewmaLong: state.SuccessesTMeanLong,
		}
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
	if state.LatencyTMeanBase != 0 || state.LatencyTMeanMid != 0 || state.LatencyTMeanLong != 0 {
		l.metrics.latency.tMean = &EWMA{
			base:     state.LatencyTMeanBase,
			ewmaMid:  state.LatencyTMeanMid,
			ewmaLong: state.LatencyTMeanLong,
		}
	}

	return l
}
