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
	}

	l := NewLevee(slo)
	if l == nil {
		t.Error("NewLevee returned nil")
	}
	if l.State() != CLOSED {
		t.Errorf("Expected initial state CLOSED, got %v", l.State())
	}
}

func TestCircuitBreakerFailureThreshold(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
	}

	l := NewLevee(slo)
	now := time.Now()

	// Fill the buffer with successful calls to establish history
	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		l.Start(ts)
		l.Success(ts, 10*time.Millisecond)
	}

	// Now simulate failures
	for i := 0; i < 100; i++ {
		ts := now.Add(time.Duration(200+i) * time.Millisecond)
		sc, _ := l.Start(ts)
		if sc.State == OPEN {
			return // Circuit opened as expected
		}
		l.Fail(ts, 10*time.Millisecond)
		if l.State() == OPEN {
			return // Circuit opened as expected
		}
	}

	t.Error("Circuit never opened despite consistent failures")
}

func TestCircuitRecovery(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.95,
		Timeout:     time.Millisecond * 100, // Short timeout for testing
	}

	l := NewLevee(slo)
	now := time.Now()

	// Fill buffer with successful calls
	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		l.Start(ts)
		l.Success(ts, 10*time.Millisecond)
	}

	// Force circuit to open with failures
	for i := 0; i < 300; i++ {
		ts := now.Add(time.Duration(200+i) * time.Millisecond)
		sc, _ := l.Start(ts)
		if sc.State == OPEN {
			break
		}
		l.Fail(ts, 10*time.Millisecond)
	}

	if l.State() != OPEN {
		t.Fatal("Circuit should be OPEN")
	}

	// Wait for timeout - circuit should allow probing call
	ts := now.Add(600 * time.Millisecond)
	sc, err := l.Start(ts)
	if sc.State != OPEN || err != nil {
		t.Errorf("Expected OPEN state after timeout with probing allowed, got %v (err=%v)", l.State(), err)
	}
	l.Success(ts, 10*time.Millisecond)

	// Circuit should allow new calls and eventually close if successful
	for i := 0; i < 100; i++ {
		ts := now.Add(time.Duration(700+i) * time.Millisecond)
		sc, _ := l.Start(ts)
		if sc.State == CLOSED {
			return // Test passed - circuit recovered
		}
		l.Success(ts, 10*time.Millisecond)
		if l.State() == CLOSED {
			return // Test passed
		}
	}

	t.Errorf("Circuit failed to recover. Last state: %v", l.State())
}

func TestMetricsReset(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
	})

	// Record some metrics
	now := time.Now()
	l.metrics.RecordLatency(100, now)
	l.metrics.RecordErrors(1, now)
	l.metrics.RecordConcurrency(5, now)

	// Open circuit which should reset metrics
	l.OpenCircuit(time.Now())

	if l.metrics.latency.Mean() != 0 ||
		l.metrics.errors.Mean() != 0 ||
		l.metrics.concurrency.Mean() != 0 {
		t.Error("Metrics were not properly reset after circuit opened")
	}
}

func TestConcurrencyTracking(t *testing.T) {
	l := NewLevee(SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second * 5,
	})

	successFunc := func() error { time.Sleep(time.Second); return nil }

	// Simulate concurrent calls
	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			l.Call(successFunc)
			done <- struct{}{}
		}()
	}

	// Wait for a moment to let concurrent calls register
	time.Sleep(time.Millisecond * 1)

	if l.Concurrents() == 0 {
		t.Error("Concurrent calls not properly tracked")
	}

	// Wait for all calls to complete
	for i := 0; i < 5; i++ {
		<-done
	}

	if l.Concurrents() != 0 {
		t.Errorf("Concurrent call counter not properly decremented, got %d", l.concurrents)
	}
}

func TestEWMACalculation(t *testing.T) {
	ts := &TimeSeries{
		values: make([]float64, 100),
		_size:  100,
		cursor: 0,
	}

	// Record consistent values
	now := time.Now()
	for i := 0; i < 100; i++ {
		ts.RecordAt(100.0, now.Add(time.Duration(i)*10*time.Millisecond))
	}

	// For consistent values, all EWMA values should be close to the input value
	tolerance := 1.0
	if abs(ts.Stat(Mean, Base)-100.0) > tolerance ||
		abs(ts.Stat(Mean, Mid)-100.0) > tolerance ||
		abs(ts.Stat(Mean, Long)-100.0) > tolerance {
		t.Errorf("EWMA values deviated too much from expected. Base: %f, Mid: %f, Long: %f",
			ts.Stat(Mean, Base), ts.Stat(Mean, Mid), ts.Stat(Mean, Long))
	}
}

var abs = math.Abs

func TestSaveStateNoHistory(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second,
	}

	l := NewLevee(slo)

	// SaveState should return nil before buffer is filled
	state, err := l.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error: %v", err)
	}
	if state != nil {
		t.Error("SaveState should return nil before EWMA is initialized")
	}
}

func TestSaveStateAfterHistory(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second,
	}

	l := NewLevee(slo)
	now := time.Now()

	// Fill the buffer to establish EWMA history
	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		l.Start(ts)
		l.Success(ts, 10*time.Millisecond)
	}

	// Now SaveState should work
	state, err := l.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error: %v", err)
	}
	if state == nil {
		t.Fatal("SaveState returned nil after establishing history")
	}

	// Verify state fields are populated
	if state.SLO.SuccessRate != slo.SuccessRate {
		t.Errorf("SLO.SuccessRate not saved correctly: got %f, want %f", state.SLO.SuccessRate, slo.SuccessRate)
	}
	if state.SLO.Timeout != slo.Timeout {
		t.Errorf("SLO.Timeout not saved correctly: got %v, want %v", state.SLO.Timeout, slo.Timeout)
	}
	if state.BufferSize == 0 {
		t.Error("BufferSize not saved")
	}
}

func TestRestoreState(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.95,
		Timeout:     500 * time.Millisecond,
	}

	// Create original levee and fill buffer
	originalLevee := NewLevee(slo)
	now := time.Now()

	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		originalLevee.Start(ts)
		if i%10 == 0 {
			// 10% error rate
			originalLevee.Fail(ts, 20*time.Millisecond)
		} else {
			originalLevee.Success(ts, 15*time.Millisecond)
		}
	}

	// Save state
	state, err := originalLevee.SaveState()
	if err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}
	if state == nil {
		t.Fatal("SaveState returned nil")
	}

	// Restore state
	restoredLevee := RestoreState(state)
	if restoredLevee == nil {
		t.Fatal("RestoreState returned nil")
	}

	// Verify EWMAs were restored
	restoredLevee.mu.RLock()
	if restoredLevee.metrics.concurrency.value == nil {
		t.Error("Concurrency value EWMA not restored")
	}
	if restoredLevee.metrics.latency.value == nil {
		t.Error("Latency value EWMA not restored")
	}
	if restoredLevee.metrics.errors.value == nil {
		t.Error("Errors value EWMA not restored")
	}

	// Verify EWMA values match saved state
	if restoredLevee.metrics.errors.value.base != state.ErrorsValueBase {
		t.Errorf("Error EWMA base not restored correctly: got %f, want %f",
			restoredLevee.metrics.errors.value.base, state.ErrorsValueBase)
	}
	if restoredLevee.metrics.errors.value.ewmaMid != state.ErrorsValueMid {
		t.Errorf("Error EWMA mid not restored correctly: got %f, want %f",
			restoredLevee.metrics.errors.value.ewmaMid, state.ErrorsValueMid)
	}
	if restoredLevee.metrics.errors.value.ewmaLong != state.ErrorsValueLong {
		t.Errorf("Error EWMA long not restored correctly: got %f, want %f",
			restoredLevee.metrics.errors.value.ewmaLong, state.ErrorsValueLong)
	}

	// Verify circuit is in CLOSED state
	if restoredLevee.state != CLOSED {
		t.Errorf("Restored Levee should be CLOSED, got %d", restoredLevee.state)
	}

	// Verify SLO was restored
	if restoredLevee.stated_slo.SuccessRate != slo.SuccessRate {
		t.Errorf("SLO.SuccessRate not restored: got %f, want %f", restoredLevee.stated_slo.SuccessRate, slo.SuccessRate)
	}
	restoredLevee.mu.RUnlock()

	// Verify restored levee can process requests
	resultState, err := restoredLevee.Start(now.Add(200 * time.Millisecond))
	if err != nil {
		t.Errorf("Restored Levee failed to process request: %v", err)
	}
	if resultState.State != CLOSED {
		t.Errorf("Restored Levee should be CLOSED, got %d", resultState.State)
	}
}

func TestStateRoundTrip(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.95,
		Timeout:     time.Second,
	}

	// Create and fill buffer
	levee1 := NewLevee(slo)
	now := time.Now()

	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		levee1.Start(ts)
		levee1.Success(ts, 50*time.Millisecond)
	}

	state1, err := levee1.SaveState()
	if err != nil || state1 == nil {
		t.Fatalf("First SaveState failed: err=%v, state=%v", err, state1)
	}

	// Restore and save again
	levee2 := RestoreState(state1)
	state2, err := levee2.SaveState()
	if err != nil || state2 == nil {
		t.Fatalf("Second SaveState failed: err=%v, state=%v", err, state2)
	}

	// Verify all EWMA values are identical
	if state1.ConcurrencyValueBase != state2.ConcurrencyValueBase ||
		state1.ConcurrencyValueMid != state2.ConcurrencyValueMid ||
		state1.ConcurrencyValueLong != state2.ConcurrencyValueLong {
		t.Error("Concurrency Value EWMAs don't match after round-trip")
	}

	if state1.ErrorsDeviationBase != state2.ErrorsDeviationBase ||
		state1.ErrorsDeviationMid != state2.ErrorsDeviationMid ||
		state1.ErrorsDeviationLong != state2.ErrorsDeviationLong {
		t.Error("Errors Deviation EWMAs don't match after round-trip")
	}
}

func TestExpunge(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second,
	}

	l := NewLevee(slo)
	now := time.Now()

	// Fill buffer and change state
	for i := 0; i < 200; i++ {
		ts := now.Add(time.Duration(i) * time.Millisecond)
		l.Start(ts)
		l.Fail(ts, 10*time.Millisecond)
	}

	// Expunge should reset to fresh state
	l.Expunge()

	if l.State() != CLOSED {
		t.Errorf("Expected CLOSED after Expunge, got %v", l.State())
	}

	// Metrics should be reset
	if l.metrics.latency.isFilled {
		t.Error("Metrics should be reset after Expunge")
	}
}

func TestCallFunction(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second,
	}

	l := NewLevee(slo)

	// Test successful call
	sc, err := l.Call(func() error { return nil })
	if err != nil {
		t.Errorf("Successful call returned error: %v", err)
	}
	if sc.State != CLOSED {
		t.Errorf("Expected CLOSED state, got %v", sc.State)
	}

	// Test failed call
	testErr := errors.New("test error")
	sc, err = l.Call(func() error { return testErr })
	if err != testErr {
		t.Errorf("Failed call did not return expected error: got %v, want %v", err, testErr)
	}
}
