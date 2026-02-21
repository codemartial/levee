package levee

import (
	"math"
	"time"
)

const (
	memMid  = 300   // Roughly, 5 minutes
	memLong = 90000 // Roughly, 1 day

	// Dynamic buffer sizing constants
	// minBufferSize = 100 ensures sufficient samples for statistical tests
	// (Wald confidence interval needs ~100 samples for reliable recovery decisions)
	minBufferSize     = 100
	maxBufferSize     = 4096
	initialBufferSize = minBufferSize
	targetFillTimeMin = 500 * time.Millisecond
	targetFillTimeMax = 1 * time.Second
	minResizeInterval = 1 * time.Second
)

type EWMA struct {
	base     float64
	ewmaMid  float64
	ewmaLong float64
}

type TimeSeries struct {
	values []float64 // Pre-allocated to maxBufferSize
	cursor uint16
	mean   float64

	value     *EWMA
	deviation *EWMA
	tMean     *EWMA // time-averaged mean: sum(values) / fillDuration

	_size    uint16 // Logical size (32-4096)
	isFilled bool

	// Dynamic sizing state
	lastResizeAt time.Time
	lastWrapAt   time.Time
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

func (s *TimeSeries) RecordAt(value float64, ts time.Time) {
	// Handle buffer full case (overwriting old value)
	if s.isFilled {
		oldValue := s.values[s.cursor]
		n := int(s._size)

		// Update mean by swapping old value for new value
		s.mean = s.mean + (value-oldValue)/float64(n)

	} else {
		// Growing phase: Welford's incremental mean
		n := int(s.cursor) + 1
		s.mean = s.mean + (value-s.mean)/float64(n)
	}

	// Write new value
	s.values[s.cursor] = value

	// Advance cursor
	s.cursor++
	if s.cursor >= s._size {
		s.cursor = 0
		s.isFilled = true

		// Capture fill duration before overwriting lastWrapAt
		var fillDuration time.Duration
		if !s.lastWrapAt.IsZero() {
			fillDuration = ts.Sub(s.lastWrapAt)
		}

		s.updateEWMAs(fillDuration)

		// Dynamic resizing on buffer wrap (after EWMA update)
		s.maybeResize(ts)
		s.lastWrapAt = ts
	}
}

// maybeResize evaluates whether the buffer should be resized
func (s *TimeSeries) maybeResize(ts time.Time) {
	// Skip if we don't have timing info yet
	if s.lastWrapAt.IsZero() {
		return
	}

	// Skip if recently resized (stability)
	if !s.lastResizeAt.IsZero() && ts.Sub(s.lastResizeAt) < minResizeInterval {
		return
	}

	// Calculate fill time for this wrap
	fillTime := ts.Sub(s.lastWrapAt)

	if fillTime < targetFillTimeMin && s._size < maxBufferSize {
		// Buffer fills too fast, grow by factor of 2
		newSize := min(s._size*2, maxBufferSize)
		s.resize(newSize)
		s.lastResizeAt = ts
	} else if fillTime > targetFillTimeMax && s._size > minBufferSize {
		// Buffer fills too slow, shrink by factor of 2
		newSize := max(s._size/2, minBufferSize)
		s.resize(newSize)
		s.lastResizeAt = ts
	}
}

// resize changes the logical buffer size without losing samples
func (s *TimeSeries) resize(newSize uint16) {
	if newSize == s._size {
		return
	}

	// EWMA values are preserved (they're already abstracted)
	// Adjust logical size but keep samples for continuity
	oldSize := s._size
	s._size = newSize

	if newSize < oldSize {
		// Shrinking: adjust cursor if it's beyond new size
		if s.cursor >= newSize {
			s.cursor = s.cursor % newSize
		}
		// If we were filled, we're still filled (just with fewer samples)
		s.recalculateMean()
	} else {
		// Growing: buffer is no longer filled until we wrap at new size
		s.isFilled = false
	}
}

// recalculateMean recalculates mean from current window after resize
func (s *TimeSeries) recalculateMean() {
	count := s.RawValueCount()
	if count == 0 {
		s.mean = 0
		return
	}

	sum := 0.0
	for i := range count {
		sum += s.values[i]
	}
	s.mean = sum / float64(count)
}

func (s *TimeSeries) ResetBase() {
	s.cursor = 0
	s.isFilled = false

	s.mean = 0

	// Reset EWMA base values while retaining mid/long history
	if s.value != nil {
		s.value.base = 0
	}
	if s.deviation != nil {
		s.deviation.base = 0
	}
	if s.tMean != nil {
		s.tMean.base = 0
	}

	s.lastWrapAt = time.Time{}
}

func (s *TimeSeries) updateEWMAs(fillDuration time.Duration) {
	alphaLo := 1.0 / float64(memMid)  // ~5 min memory
	alphaHi := 1.0 / float64(memLong) // ~1 day memory

	s.value = s.value.update(s.mean, alphaLo, alphaHi)

	// Mean absolute deviation
	count := s.RawValueCount()
	sumAD := 0.0
	for i := range count {
		sumAD += math.Abs(s.values[i] - s.mean)
	}
	s.deviation = s.deviation.update(sumAD/float64(count), alphaLo, alphaHi)

	// TMean: rate of accumulation (sum of values / fill duration)
	if fillDuration > 0 {
		deriv := s.mean * float64(s._size) / fillDuration.Seconds()
		s.tMean = s.tMean.update(deriv, alphaLo, alphaHi)
	}
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
	TMean
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
	case TMean:
		stat = s.tMean
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
	successes TimeSeries
	latency   TimeSeries
}

func newMetrics(size uint16) *metrics {
	// Pre-allocate to maxBufferSize for all buffers
	return &metrics{
		successes: TimeSeries{values: make([]float64, maxBufferSize), _size: size},
		latency:   TimeSeries{values: make([]float64, maxBufferSize), _size: size},
	}
}

func (m *metrics) RecordSuccesses(successes float64, ts time.Time) {
	m.successes.RecordAt(successes, ts)
}

func (m *metrics) RecordLatency(latency float64, ts time.Time) {
	m.latency.RecordAt(latency, ts)
}

func (m *metrics) Reset() {
	m.successes.ResetBase()
	m.latency.ResetBase()
}

// hasSufficientHistory returns true if EWMA history has been established
// for successes and latency metrics (required for anomaly detection)
func (m *metrics) hasSufficientHistory() bool {
	return m.successes.tMean != nil && m.latency.tMean != nil
}
