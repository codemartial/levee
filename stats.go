package levee

import (
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
	values  []float64
	mean    float64
	sumAD   float64
	sumVT   float64
	sumTT   float64
	delta_t float64

	value      *EWMA
	p99        *EWMA
	deviation  *EWMA
	derivative *EWMA

	_size uint16
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
	if len(s.values) == 0 {
		s.delta_t = float64(t.UnixMicro())
	}

	s.values = append(s.values, value)
	s.mean = s.mean + (value-s.mean)/float64(len(s.values))
	s.sumAD = s.sumAD + (value - s.mean)

	normalized_t := float64(t.UnixMicro()) - s.delta_t
	s.sumVT = s.sumVT + value*normalized_t
	s.sumTT = s.sumTT + normalized_t*normalized_t

	if len(s.values) == cap(s.values) && cap(s.values) > 0 {
		s.updateEWMAs()
		s.values = s.values[:0]
	}
}

func (s *TimeSeries) ResetBase() {
	s.values = s.values[:0]
	s.mean = 0
	s.sumAD = 0
	s.sumVT = 0
	s.sumTT = 0
	s.delta_t = 0
}

func (s *TimeSeries) updateEWMAs() {
	// Normalize alpha based on sample count and memory window
	alphaLo := 1.0 / float64(s._size) / memMid
	alphaHi := 1.0 / float64(s._size) / memLong

	s.value = s.value.update(s.mean, alphaLo, alphaHi)

	sort.Float64s(s.values)
	i_99 := len(s.values) * 99 / 100
	p99 := s.values[i_99]
	s.p99 = s.p99.update(p99, alphaLo, alphaHi)

	deviation := s.sumAD / float64(len(s.values))
	s.deviation = s.deviation.update(deviation, alphaLo, alphaHi)

	derivative := s.sumTT / s.sumVT // Least squares method
	s.derivative = s.derivative.update(derivative, alphaLo, alphaHi)
}

func (s *TimeSeries) RawValueCount() int {
	return len(s.values)
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
	sampleSize := float64(len(s.values))
	if len(s.values) == 0 {
		if s.value == nil {
			return 0
		}
		return s.value.base // If no current data, rely entirely on historical mean
	}

	if s.value == nil {
		return s.mean
	}

	// Adjust weights based on MAD
	historicalWeight := float64(s._size)
	currentWeight := sampleSize * sampleSize / (s.sumAD + 1e-9) // Lower MAD increases weight

	// Weighted average
	bestGuess := (float64(s._size)*s.value.base + currentWeight*s.mean) / (historicalWeight + currentWeight)
	return bestGuess
}

func (s *TimeSeries) Deviation() float64 {
	if len(s.values) == 0 {
		return 0
	}
	return s.sumAD / float64(len(s.values))
}

type metrics struct {
	concurrency TimeSeries
	latency     TimeSeries
	errors      TimeSeries
	requests    TimeSeries
}

func newMetrics(size uint16) *metrics {
	return &metrics{
		concurrency: TimeSeries{values: make([]float64, 0, size), _size: size},
		latency:     TimeSeries{values: make([]float64, 0, size), _size: size},
		errors:      TimeSeries{values: make([]float64, 0, size), _size: size},
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
