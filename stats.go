package levee

import (
	"math"
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

	value     *EWMA
	deviation *EWMA

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

func (s *TimeSeries) Record(value float64) {
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

	// Reset EWMA base values while retaining mid/long history
	if s.value != nil {
		s.value.base = 0
	}
	if s.deviation != nil {
		s.deviation.base = 0
	}
}

func (s *TimeSeries) updateEWMAs() {
	alphaLo := 1.0 / float64(memMid)  // ~5 min memory
	alphaHi := 1.0 / float64(memLong) // ~1 day memory

	s.value = s.value.update(s.mean, alphaLo, alphaHi)

	// Deviation computation - ensure sumAD is current
	s.ensureSumAD()
	deviation := s.sumAD / float64(s.RawValueCount())
	s.deviation = s.deviation.update(deviation, alphaLo, alphaHi)
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
	Mean StatType = iota
	Deviation
)

type StatRange uint8

const (
	Base StatRange = iota
	Mid
	Long
)

func (s *TimeSeries) Stat(st StatType, sr StatRange) float64 {
	var stat *EWMA
	switch st {
	case Mean:
		stat = s.value
	case Deviation:
		stat = s.deviation
	}

	if stat == nil {
		return 0
	}

	switch sr {
	case Base:
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

type metrics struct {
	concurrency TimeSeries
	latency     TimeSeries
	errors      TimeSeries
}

func newMetrics(size uint16) *metrics {
	return &metrics{
		concurrency: TimeSeries{values: make([]float64, size), _size: size},
		latency:     TimeSeries{values: make([]float64, size), _size: size},
		errors:      TimeSeries{values: make([]float64, size), _size: size},
	}
}

func (m *metrics) RecordConcurrency(concurrency float64) {
	m.concurrency.Record(concurrency)
}

func (m *metrics) RecordLatency(latency float64) {
	m.latency.Record(latency)
}

func (m *metrics) RecordErrors(err float64) {
	m.errors.Record(err)
}

func (m *metrics) Reset() {
	m.concurrency.ResetBase()
	m.latency.ResetBase()
	m.errors.ResetBase()
}

// hasSufficientHistory returns true if EWMA history has been established
// for concurrency and latency metrics (required for anomaly detection)
func (m *metrics) hasSufficientHistory() bool {
	return m.concurrency.isFilled &&
		m.latency.isFilled &&
		m.concurrency.value != nil &&
		m.latency.value != nil
}
