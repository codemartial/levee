package levee

import (
	"math"
	"sort"
	"time"
)

const (
	memMid  = 300   // Roughly, 5 minutes
	memLong = 90000 // Roughly, 1 day
)

type EWMA struct {
	base     float64
	ewmaMid  float64
	ewmaLong float64
}

type TimeSeries struct {
	values     []float64
	cursor     uint16
	mean       float64
	sumAD      float64
	sumADStale bool
	sumVT      float64
	sumTT      float64
	delta_t    float64

	value      *EWMA
	p99        *EWMA
	deviation  *EWMA
	derivative *EWMA

	_size    uint16
	isFilled bool
}

func (ma *EWMA) update(value, alphaLo, alphaHi float64) *EWMA {
	if ma == nil { // EWMA never initialized
		ma = &EWMA{
			base:     value,
			ewmaMid:  value,
			ewmaLong: value,
		}
	} else {
		ma.base = value
		ma.ewmaMid = (1-alphaLo)*ma.ewmaMid + alphaLo*value
		ma.ewmaLong = (1-alphaHi)*ma.ewmaLong + alphaHi*value
	}
	return ma
}

func (s *TimeSeries) Record(value float64, t time.Time) {
	// Initialize or reset timestamp baseline on wrap
	if s.cursor == 0 {
		s.delta_t = float64(t.UnixMicro())
		// Reset time-based sums on wrap (can't maintain without timestamp buffer)
		if s.isFilled {
			s.sumVT = 0
			s.sumTT = 0
		}
	}

	normalized_t := float64(t.UnixMicro()) - s.delta_t

	// Handle buffer full case (overwriting old value)
	if s.isFilled {
		oldValue := s.values[s.cursor]
		n := int(s._size)

		// Update mean by swapping old value for new value
		s.mean = s.mean + (value-oldValue)/float64(n)

		// Mark sumAD as stale (will recompute when needed)
		s.sumADStale = true
	} else {
		// Growing phase: incremental updates
		n := int(s.cursor) + 1 // New sample count

		// Welford's incremental mean
		s.mean = s.mean + (value-s.mean)/float64(n)

		// Mark sumAD as stale
		s.sumADStale = true
	}

	// Accumulate time-based sums (these will be reset on wrap)
	s.sumVT = s.sumVT + value*normalized_t
	s.sumTT = s.sumTT + normalized_t*normalized_t

	// Write new value
	s.values[s.cursor] = value

	// Advance cursor
	s.cursor++
	shouldUpdateEWMA := false

	if s.cursor >= s._size {
		s.cursor = 0
		s.isFilled = true // Mark buffer as filled on first wrap and subsequent wraps
		shouldUpdateEWMA = true
	}

	// Update EWMAs on buffer wrap (both first fill and subsequent wraps)
	if shouldUpdateEWMA && s._size > 0 {
		s.updateEWMAs()
	}
}

func (s *TimeSeries) ResetBase() {
	s.cursor = 0
	s.isFilled = false

	s.mean = 0
	s.sumAD = 0
	s.sumADStale = false
	s.sumVT = 0
	s.sumTT = 0
	s.delta_t = 0

	// Reset EWMA base values while retaining mid/long history
	if s.value != nil {
		s.value.base = 0
	}
	if s.p99 != nil {
		s.p99.base = 0
	}
	if s.deviation != nil {
		s.deviation.base = 0
	}
	if s.derivative != nil {
		s.derivative.base = 0
	}
}

func (s *TimeSeries) updateEWMAs() {
	alphaLo := 1.0 / float64(memMid)  // ~5 min memory
	alphaHi := 1.0 / float64(memLong) // ~1 day memory

	s.value = s.value.update(s.mean, alphaLo, alphaHi)

	// For P99: need to sort values, but can't modify ring buffer in place
	// Create a temporary copy
	count := s.RawValueCount()
	sortedValues := make([]float64, count)
	copy(sortedValues, s.values[:count])

	sort.Float64s(sortedValues)
	i_99 := len(sortedValues) * 99 / 100
	p99 := sortedValues[i_99]
	s.p99 = s.p99.update(p99, alphaLo, alphaHi)

	// Deviation computation - ensure sumAD is current
	s.ensureSumAD()
	deviation := s.sumAD / float64(count)
	s.deviation = s.deviation.update(deviation, alphaLo, alphaHi)

	// Derivative using least squares method
	// This gives the rate of change of value over time.
	// Handle division by zero: if sumTT is zero/tiny, derivative is undefined (use 0)
	var derivative float64
	if math.Abs(s.sumTT) > 1e-9 {
		derivative = s.sumVT / s.sumTT
	} else {
		derivative = 0 // No meaningful rate of change
	}
	s.derivative = s.derivative.update(derivative, alphaLo, alphaHi)
}

func (s *TimeSeries) ensureSumAD() {
	if !s.sumADStale {
		return
	}

	s.sumAD = 0
	count := s.RawValueCount()

	for i := range count {
		s.sumAD += math.Abs(s.values[i] - s.mean)
	}

	s.sumADStale = false
}

func (s *TimeSeries) RawValueCount() int {
	// Buffer is filled if we've wrapped around at least once
	if s.isFilled {
		return int(s._size)
	}
	return int(s.cursor)
}

type StatType uint8

const (
	Derivative StatType = iota
	Mean
	P99
	Deviation
)

type StatRange uint8

const (
	Raw StatRange = iota
	Mid
	Long
)

func (s *TimeSeries) Stat(st StatType, sr StatRange) float64 {
	var stat *EWMA
	switch st {
	case Derivative:
		stat = s.derivative
	case Mean:
		stat = s.value
	case P99:
		stat = s.p99
	case Deviation:
		stat = s.deviation
	}

	if stat == nil {
		return 0
	}

	switch sr {
	case Raw:
		return stat.base
	case Mid:
		return stat.ewmaMid
	case Long:
		return stat.ewmaLong
	default:
		return 0
	}
}

func (s *TimeSeries) Mean() float64 {
	return s.mean
}

func (s *TimeSeries) Deviation() float64 {
	if s.RawValueCount() == 0 {
		return 0
	}
	s.ensureSumAD() // Lazy computation
	return s.sumAD / float64(s.RawValueCount())
}

type metrics struct {
	concurrency TimeSeries
	latency     TimeSeries
	errors      TimeSeries
	requests    TimeSeries
}

func newMetrics(size uint16) *metrics {
	return &metrics{
		concurrency: TimeSeries{values: make([]float64, size), _size: size, cursor: 0},
		latency:     TimeSeries{values: make([]float64, size), _size: size, cursor: 0},
		errors:      TimeSeries{values: make([]float64, size), _size: size, cursor: 0},
		requests:    TimeSeries{values: make([]float64, size), _size: size, cursor: 0},
	}
}

func (m *metrics) RecordConcurrency(concurrency float64, t time.Time) {
	m.concurrency.Record(concurrency, t)
}

func (m *metrics) RecordLatency(latency float64, t time.Time) {
	m.latency.Record(latency, t)
}

func (m *metrics) RecordErrors(err float64, t time.Time) {
	m.errors.Record(err, t)
}

func (m *metrics) RecordRequests(requests float64, t time.Time) {
	m.requests.Record(requests, t)
}

func (m *metrics) ConfidenceInterval() float64 {
	return 0
}

func (m *metrics) Reset() {
	m.concurrency.ResetBase()
	m.latency.ResetBase()
	m.errors.ResetBase()
	m.requests.ResetBase()
}

// hasSufficientHistory returns true if EWMA history has been established
// for concurrency and latency metrics (required for anomaly detection)
func (m *metrics) hasSufficientHistory() bool {
	return m.concurrency.isFilled &&
		m.latency.isFilled &&
		m.concurrency.value != nil &&
		m.latency.value != nil
}
