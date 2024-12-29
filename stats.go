package levee

import (
	"slices"
	"sort"
	"time"
)

const (
	extLo = 300   // Roughly, 5 minutes
	extHi = 90000 // Roughly, 1 day
)

type EWMA struct {
	base   float64
	ewmaLo float64
	ewmaHi float64
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
	alphaLo := 1.0 / float64(s._size) / extLo
	alphaHi := 1.0 / float64(s._size) / extHi

	if s.value == nil { // EWMA never initialized
		s.value = &EWMA{
			base:   s.mean,
			ewmaLo: s.mean,
			ewmaHi: s.mean,
		}
	} else {
		s.value.base = s.mean
		s.value.ewmaLo = (1-alphaLo)*s.value.ewmaLo + alphaLo*s.mean
		s.value.ewmaHi = (1-alphaHi)*s.value.ewmaHi + alphaHi*s.mean
	}

	sorted := slices.Clone(s.values)
	sort.Float64s(sorted)
	i_99 := int(float64(len(s.values)) * 0.99)
	p99 := sorted[i_99]

	if s.p99 == nil {
		s.p99 = &EWMA{
			base:   p99,
			ewmaLo: p99,
			ewmaHi: p99,
		}
	} else {
		s.p99.base = p99
		s.p99.ewmaLo = (1-alphaLo)*s.p99.ewmaLo + alphaLo*p99
		s.p99.ewmaHi = (1-alphaHi)*s.p99.ewmaHi + alphaHi*p99
	}

	if s.deviation == nil {
		s.deviation = &EWMA{
			base:   s.sumAD / float64(len(s.values)),
			ewmaLo: s.sumAD / float64(len(s.values)),
			ewmaHi: s.sumAD / float64(len(s.values)),
		}
	} else {
		s.deviation.base = s.sumAD / float64(len(s.values))
		s.deviation.ewmaLo = (1-alphaLo)*s.deviation.ewmaLo + alphaLo*s.sumAD/float64(len(s.values))
		s.deviation.ewmaHi = (1-alphaHi)*s.deviation.ewmaHi + alphaHi*s.sumAD/float64(len(s.values))
	}

	derivative := s.Derivative()

	if s.derivative == nil {
		s.derivative = &EWMA{
			base:   derivative,
			ewmaLo: derivative,
			ewmaHi: derivative,
		}
	} else {
		s.derivative.base = derivative
		s.derivative.ewmaLo = (1-alphaLo)*s.derivative.ewmaLo + alphaLo*derivative
		s.derivative.ewmaHi = (1-alphaHi)*s.derivative.ewmaHi + alphaHi*derivative
	}
}

func (s *TimeSeries) FillRate() float64 {
	return float64(len(s.values)) / float64(cap(s.values))
}

// Use the least squares method to calculate the derivative of the series
func (s *TimeSeries) Derivative() float64 {
	sumXX := s.sumTT
	sumXY := s.sumVT

	return sumXY / sumXX
}

func (s *TimeSeries) DerivativeBase() float64 {
	if s.derivative == nil {
		return 0
	}
	return s.derivative.base
}

func (s *TimeSeries) DerivativeMid() float64 {
	if s.derivative == nil {
		return 0
	}
	return s.derivative.ewmaLo
}

func (s *TimeSeries) DerivativeLong() float64 {
	if s.derivative == nil {
		return 0
	}
	return s.derivative.ewmaHi
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

func (s *TimeSeries) MeanBase() float64 {
	if s.value == nil {
		return 0
	}
	return s.value.base
}

func (s *TimeSeries) MeanMid() float64 {
	if s.value == nil {
		return 0
	}
	return s.value.ewmaLo
}

func (s *TimeSeries) MeanLong() float64 {
	if s.value == nil {
		return 0
	}
	return s.value.ewmaHi
}

func (s *TimeSeries) P99Base() float64 {
	if s.p99 == nil {
		return 0
	}
	return s.p99.base
}

func (s *TimeSeries) P99Mid() float64 {
	if s.p99 == nil {
		return 0
	}
	return s.p99.ewmaLo
}

func (s *TimeSeries) P99Long() float64 {
	if s.p99 == nil {
		return 0
	}
	return s.p99.ewmaHi
}

func (s *TimeSeries) Deviation() float64 {
	if len(s.values) == 0 {
		return 0
	}
	return s.sumAD / float64(len(s.values))
}

func (s *TimeSeries) DeviationBase() float64 {
	if s.deviation == nil {
		return 0
	}
	return s.deviation.base
}

func (s *TimeSeries) DeviationMid() float64 {
	if s.deviation == nil {
		return 0
	}
	return s.deviation.ewmaLo
}

func (s *TimeSeries) DeviationLong() float64 {
	if s.deviation == nil {
		return 0
	}
	return s.deviation.ewmaHi
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
