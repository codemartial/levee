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

	value      EWMA
	p99        EWMA
	deviation  EWMA
	derivative EWMA

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

	if len(s.values) == cap(s.values) {
		s.updateEWMAs()
	}
}

func (s *TimeSeries) updateEWMAs() {
	alphaLo := 1.0 / float64(s._size) * extLo
	alphaHi := 1.0 / float64(s._size) * extHi

	s.value.ewmaLo = (1-alphaLo)*s.value.ewmaLo + alphaLo*s.mean
	s.value.ewmaHi = (1-alphaHi)*s.value.ewmaHi + alphaHi*s.mean

	sorted := slices.Clone(s.values)
	sort.Float64s(sorted)
	p99 := sorted[(99*len(s.values))/100]
	s.p99.ewmaLo = (1-alphaLo)*s.p99.ewmaLo + alphaLo*p99
	s.p99.ewmaHi = (1-alphaHi)*s.p99.ewmaHi + alphaHi*p99

	s.deviation.ewmaLo = (1-alphaLo)*s.deviation.ewmaLo + alphaLo*s.sumAD/float64(len(s.values))
	s.deviation.ewmaHi = (1-alphaHi)*s.deviation.ewmaHi + alphaHi*s.sumAD/float64(len(s.values))

	derivative := s.Derivative()
	s.derivative.ewmaLo = (1-alphaLo)*s.derivative.ewmaLo + alphaLo*derivative
	s.derivative.ewmaHi = (1-alphaHi)*s.derivative.ewmaHi + alphaHi*derivative
}

// Use the least squares method to calculate the derivative of the series
func (s *TimeSeries) Derivative() float64 {
	sumXX := s.sumTT
	sumXY := s.sumVT

	return sumXY / sumXX
}

func (s *TimeSeries) DerivativeLo() float64 {
	return s.derivative.ewmaLo
}

func (s *TimeSeries) DerivativeHi() float64 {
	return s.derivative.ewmaHi
}

func (s *TimeSeries) Mean() float64 {
	return s.mean
}

func (s *TimeSeries) MeanLo() float64 {
	return s.value.ewmaLo
}

func (s *TimeSeries) MeanHi() float64 {
	return s.value.ewmaHi
}

func (s *TimeSeries) P99Lo() float64 {
	return s.p99.ewmaLo
}

func (s *TimeSeries) P99Hi() float64 {
	return s.p99.ewmaHi
}

func (s *TimeSeries) Deviation() float64 {
	return s.sumAD / float64(len(s.values))
}

func (s *TimeSeries) DeviationLo() float64 {
	return s.deviation.ewmaLo
}

func (s *TimeSeries) DeviationHi() float64 {
	return s.deviation.ewmaHi
}

type metrics struct {
	concurrents uint32
	concurrency TimeSeries
	latency     TimeSeries
	errors      TimeSeries
}

func newMetrics(size uint16) *metrics {
	return &metrics{
		concurrency: TimeSeries{values: make([]float64, 0, size), _size: size},
		latency:     TimeSeries{values: make([]float64, 0, size), _size: size},
		errors:      TimeSeries{values: make([]float64, 0, size), _size: size},
	}
}

func (m *metrics) Record(concurrency float64, latency float64, err float64, t time.Time) {
	m.concurrency.Record(concurrency, t)
	m.latency.Record(latency, t)
	m.errors.Record(err, t)
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
