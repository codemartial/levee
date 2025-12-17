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
	successFunc := func() error { return nil }

	// Start making calls immediately - first call starts warmup timer
	// Make calls during warmup period (these don't count toward 1000)
	for i := 0; i < 100; i++ {
		l.Call(successFunc)
	}

	// Wait for warmup period to complete from first call
	time.Sleep(slo.Warmup + time.Millisecond*100)

	// Now make 1001+ successful calls after warmup - these should count
	// Need enough to trigger transition (>1000 in CLOSED state)
	for i := 0; i < 1100; i++ {
		state, err := l.Call(successFunc)
		if err != nil {
			t.Errorf("Unexpected error during warmup: %v", err)
		}
		// After warmup completes, state should eventually be CLOSED
		if i == 1099 && state != CLOSED {
			t.Errorf("Expected CLOSED state after warmup+1000 calls, got %v", state)
		}
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
	for i := 0; i < 300; i++ {
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
	cb.metrics.RecordLatency(100)
	cb.metrics.RecordErrors(1)
	cb.metrics.RecordConcurrency(5)

	// Open circuit which should reset metrics
	cb.OpenCircuit(time.Now())

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
		values: make([]float64, 100),
		_size:  100,
		cursor: 0,
	}

	// Record consistent values
	for i := 0; i < 100; i++ {
		ts.Record(100.0)
	}

	// For consistent values, all EWMA values should be close to the input value
	tolerance := 1.0
	if abs(ts.Stat(Mean, Raw)-100.0) > tolerance ||
		abs(ts.Stat(Mean, Mid)-100.0) > tolerance ||
		abs(ts.Stat(Mean, Long)-100.0) > tolerance {
		t.Errorf("EWMA values deviated too much from expected. Base: %f, Mid: %f, Long: %f",
			ts.Stat(Mean, Raw), ts.Stat(Mean, Mid), ts.Stat(Mean, Long))
	}
}

var abs = math.Abs

func TestSaveStateBeforeWarmup(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.99,
		Timeout:     time.Second,
		Warmup:      10,
	}

	levee := NewLevee(slo)

	// SaveState should return nil before warmup completes
	state, err := levee.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error: %v", err)
	}
	if state != nil {
		t.Error("SaveState should return nil during warmup phase")
	}
}

func TestSaveStateAfterWarmup(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.5, // Low success rate: 10/(1-0.5) = 20 samples
		Timeout:     time.Second,
		Warmup:      1 * time.Second,
	}

	levee := NewLevee(slo)
	now := time.Now()

	// Low RPS (1 req/sec) + low success rate = buffer size 100 (max of ~1, 100, 20)
	// Need ~1000 requests to transition from WarmupCB, then 100+ to fill buffer
	for i := 0; i < 1500; i++ {
		ts := now.Add(time.Duration(i) * time.Second) // 1s spacing = ~1 RPS
		levee.Start(ts)
		levee.Success(ts, 10*time.Millisecond)
	}

	// Now SaveState should work
	state, err := levee.SaveState()
	if err != nil {
		t.Fatalf("SaveState returned error after warmup: %v", err)
	}
	if state == nil {
		// Check if levee is ready and if EWMAs are initialized
		levee.mu.RLock()
		ready := levee.ready
		cb, _ := levee.cb.(*CircuitBreaker)
		var bufferSize uint16
		var hasEWMA bool
		if cb != nil {
			cb.mu.RLock()
			bufferSize = cb.metrics.concurrency._size
			hasEWMA = cb.metrics.concurrency.value != nil
			cb.mu.RUnlock()
		}
		levee.mu.RUnlock()
		t.Fatalf("SaveState returned nil (ready=%v, bufferSize=%d, hasEWMA=%v)", ready, bufferSize, hasEWMA)
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
		SuccessRate: 0.5,
		Timeout:     500 * time.Millisecond,
		Warmup:      1 * time.Second,
	}

	// Create and warm up original levee
	originalLevee := NewLevee(slo)
	now := time.Now()

	// Wide spacing for small buffer, need 1500+ requests
	for i := 0; i < 1500; i++ {
		ts := now.Add(time.Duration(i) * time.Second) // 1s spacing
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

	// Verify the restored levee is in ready state (not warmup)
	restoredLevee.mu.RLock()
	if !restoredLevee.ready {
		t.Error("Restored Levee should be in ready state")
	}
	cb, ok := restoredLevee.cb.(*CircuitBreaker)
	restoredLevee.mu.RUnlock()

	if !ok {
		t.Fatal("Restored Levee should have CircuitBreaker, not WarmupCB")
	}

	// Verify EWMAs were restored
	cb.mu.RLock()
	if cb.metrics.concurrency.value == nil {
		t.Error("Concurrency value EWMA not restored")
	}
	if cb.metrics.latency.value == nil {
		t.Error("Latency value EWMA not restored")
	}
	if cb.metrics.errors.value == nil {
		t.Error("Errors value EWMA not restored")
	}

	// Verify EWMA values match saved state
	if cb.metrics.errors.value.base != state.ErrorsValueBase {
		t.Errorf("Error EWMA base not restored correctly: got %f, want %f",
			cb.metrics.errors.value.base, state.ErrorsValueBase)
	}
	if cb.metrics.errors.value.ewmaMid != state.ErrorsValueMid {
		t.Errorf("Error EWMA mid not restored correctly: got %f, want %f",
			cb.metrics.errors.value.ewmaMid, state.ErrorsValueMid)
	}
	if cb.metrics.errors.value.ewmaLong != state.ErrorsValueLong {
		t.Errorf("Error EWMA long not restored correctly: got %f, want %f",
			cb.metrics.errors.value.ewmaLong, state.ErrorsValueLong)
	}

	// Verify circuit is in CLOSED state
	if cb.state != CLOSED {
		t.Errorf("Restored CircuitBreaker should be CLOSED, got %d", cb.state)
	}

	// Verify SLO was restored
	if cb.stated_slo.SuccessRate != slo.SuccessRate {
		t.Errorf("SLO.SuccessRate not restored: got %f, want %f", cb.stated_slo.SuccessRate, slo.SuccessRate)
	}
	cb.mu.RUnlock()

	// Verify restored levee can process requests
	resultState, err := restoredLevee.Start(now.Add(200 * time.Millisecond))
	if err != nil {
		t.Errorf("Restored Levee failed to process request: %v", err)
	}
	if resultState != CLOSED {
		t.Errorf("Restored Levee should be CLOSED, got %d", resultState)
	}
}

func TestStateRoundTrip(t *testing.T) {
	slo := SLO{
		SuccessRate: 0.5,
		Timeout:     time.Second,
		Warmup:      1 * time.Second,
	}

	// Create, warm up, and save
	levee1 := NewLevee(slo)
	now := time.Now()

	// Wide spacing for small buffer, need 1500+ requests
	for i := 0; i < 1500; i++ {
		ts := now.Add(time.Duration(i) * time.Second) // 1s spacing
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
