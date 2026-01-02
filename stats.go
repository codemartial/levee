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
	initialBufferSize = 100
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
	values     []float64 // Pre-allocated to maxBufferSize
	cursor     uint16
	mean       float64
	sumAD      float64
	sumADStale bool

	value     *EWMA
	deviation *EWMA

	_size    uint16 // Logical size (32-4096)
	isFilled bool

	// Dynamic sizing state
	lastResizeAt     time.Time
	lastWrapAt       time.Time
	recordsSinceWrap uint32
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
	s.recordsSinceWrap++
	shouldUpdateEWMA := false

	if s.cursor >= s._size {
		s.cursor = 0
		s.isFilled = true
		shouldUpdateEWMA = true

		// Dynamic resizing on buffer wrap
		s.maybeResize(ts)

		// Reset wrap tracking
		s.lastWrapAt = ts
		s.recordsSinceWrap = 0
	}

	// Update EWMAs on buffer wrap
	if shouldUpdateEWMA && s._size > 0 {
		s.updateEWMAs()
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

	if fillTime < targetFillTimeMin && s._size > minBufferSize {
		// Buffer fills too fast, grow by factor of 2
		newSize := min(s._size*2, maxBufferSize)
		s.resize(newSize)
		s.lastResizeAt = ts
	} else if fillTime > targetFillTimeMax && s._size < maxBufferSize {
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
		// Mark stats as stale to recalculate from new window
		s.sumADStale = true
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
	s.sumAD = 0
	s.sumADStale = false

	// Reset EWMA base values while retaining mid/long history
	if s.value != nil {
		s.value.base = 0
	}
	if s.deviation != nil {
		s.deviation.base = 0
	}

	// Reset timing state
	s.lastWrapAt = time.Time{}
	s.recordsSinceWrap = 0
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
	// Pre-allocate to maxBufferSize for all buffers
	return &metrics{
		concurrency: TimeSeries{values: make([]float64, maxBufferSize), _size: size},
		latency:     TimeSeries{values: make([]float64, maxBufferSize), _size: size},
		errors:      TimeSeries{values: make([]float64, maxBufferSize), _size: size},
	}
}

func (m *metrics) RecordConcurrency(concurrency float64, ts time.Time) {
	m.concurrency.RecordAt(concurrency, ts)
}

func (m *metrics) RecordLatency(latency float64, ts time.Time) {
	m.latency.RecordAt(latency, ts)
}

func (m *metrics) RecordErrors(err float64, ts time.Time) {
	m.errors.RecordAt(err, ts)
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
