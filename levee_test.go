package levee

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewLevee(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
		Warmup:      time.Second * 10,
	}

	l := NewLevee(slo)
	if l == nil {
		t.Error("NewLevee returned nil")
	}
	if l.State() != INIT {
		t.Errorf("Expected initial state INIT, got %v", l.State())
	}
}

func TestWarmupPhase(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
		Warmup:      time.Second * 1,
	}

	l := NewLevee(slo)
	// Simulate successful calls during warmup
	successFunc := func() error { return nil }

	// Wait for warmup period to complete
	time.Sleep(slo.Warmup)

	// Make 1000 successful calls after warmup period
	for i := 0; i < 1000; i++ {
		state, err := l.Call(successFunc)
		if err != nil {
			t.Errorf("Unexpected error during warmup: %v", err)
		}
		if i < 999 && state == CLOSED {
			t.Error("Circuit closed before request count completion")
		}
	}

	// After enough requests, one more call should transition to normal operation
	state, err := l.Call(successFunc)
	if err != nil {
		t.Errorf("Unexpected error after warmup: %v", err)
	}
	if state != CLOSED {
		t.Errorf("Expected CLOSED state after warmup, got %v", state)
	}
}

func TestCircuitBreakerFailureThreshold(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
		Warmup:      time.Second * 1,
	}

	l := NewLevee(slo)

	// Wait for warmup period to complete
	time.Sleep(slo.Warmup)

	// Complete warmup phase
	successFunc := func() error { return nil }
	for i := 0; i < 1001; i++ {
		s, _ := l.Call(successFunc)
		if s == CLOSED {
			break
		}
	}

	// Now simulate failures
	failureFunc := func() error { return errors.New("test error") }

	// Record enough failures to potentially trigger circuit opening
	for i := 0; i < 100; i++ {
		state, _ := l.Call(failureFunc)
		if state == OPEN {
			// Circuit should eventually open due to failures
			return
		}
	}

	t.Error("Circuit never opened despite consistent failures")
}

func TestCircuitRecovery(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.95,
		Timeout:     time.Millisecond * 100, // Short timeout for testing
		Warmup:      time.Second * 0,
	}

	l := NewLevee(slo)

	// Complete warmup
	successFunc := func() error { return nil }
	for i := 0; i < 1001; i++ {
		state, _ := l.Call(successFunc)
		if state == CLOSED {
			break
		}
	}

	// Force circuit to open
	failureFunc := func() error { return errors.New("test error") }
	for i := 0; i < 100; i++ {
		state, _ := l.Call(failureFunc)
		if state == OPEN {
			break
		}
	}

	// Wait for timeout
	time.Sleep(slo.Timeout)

	if state, err := l.Call(successFunc); state != HALF_OPEN || err != nil {
		t.Errorf("Expected HALF_OPEN state after timeout, got %v", l.State())
	}

	// Circuit should allow new calls and eventually close if successful
	var lastState State
	var lastErr error

	for i := 0; i < 100; i++ {
		lastState, lastErr = l.Call(successFunc)
		if lastState == CLOSED {
			return // Test passed - circuit recovered
		}
	}

	t.Errorf("Circuit failed to recover. Last state: %v, Last error: %v", lastState, lastErr)
}

func TestMetricsReset(t *testing.T) {
	cb := NewCircuitBreaker(SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
	}, 100)

	// Record some metrics
	cb.metrics.RecordLatency(100, time.Now())
	cb.metrics.RecordErrors(1, time.Now())
	cb.metrics.RecordConcurrency(5, time.Now())

	// Open circuit which should reset metrics
	cb.OpenCircuit()

	if cb.metrics.latency.Mean() != 0 ||
		cb.metrics.errors.Mean() != 0 ||
		cb.metrics.concurrency.Mean() != 0 {
		t.Error("Metrics were not properly reset after circuit opened")
	}
}

func TestConcurrencyTracking(t *testing.T) {
	cb := NewCircuitBreaker(SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
	}, 100)

	successFunc := func() error { time.Sleep(time.Second); return nil }

	// Simulate concurrent calls
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			cb.Call(successFunc)
			done <- struct{}{}
		}()
	}

	// Wait for a moment to let concurrent calls register
	time.Sleep(time.Millisecond * 1)

	if cb.Concurrents() == 0 {
		t.Error("Concurrent calls not properly tracked")
	}

	// Wait for all calls to complete
	for i := 0; i < 5; i++ {
		<-done
	}

	if cb.Concurrents() != 0 {
		t.Errorf("Concurrent call counter not properly decremented, got %d", cb.concurrents)
	}
}

func TestEWMACalculation(t *testing.T) {
	ts := &TimeSeries{
		values: make([]float64, 0, 100),
		_size:  100,
	}

	// Record consistent values
	now := time.Now()
	for i := 0; i < 100; i++ {
		ts.Record(100.0, now.Add(time.Duration(i)*time.Second))
	}

	// For consistent values, all EWMA values should be close to the input value
	tolerance := 1.0
	if abs(ts.MeanBase()-100.0) > tolerance ||
		abs(ts.MeanMid()-100.0) > tolerance ||
		abs(ts.MeanLong()-100.0) > tolerance {
		t.Errorf("EWMA values deviated too much from expected. Base: %f, Mid: %f, Long: %f",
			ts.MeanBase(), ts.MeanMid(), ts.MeanLong())
	}
}

var abs = math.Abs
