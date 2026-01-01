package levee

import (
	"math"
)

func (l *Levee) allowCall() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.state != OPEN || !l.cooldownComplete {
		panic("Bug Encountered. This method must only be called during OPEN probing phase")
	}

	// historicals
	hErrors := l.metrics.errors.Stat(Mean, Mid)
	hConcurrency := l.metrics.concurrency.Stat(Mean, Mid)

	var allowedConcurrency int32 = 1
	if hConcurrency > 0 {
		allowedConcurrency = max(1, int32((1-hErrors)*hConcurrency))
	}

	return l.concurrents <= allowedConcurrency
}

func (l *Levee) newState() (State, Trigger) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.state != OPEN || !l.cooldownComplete {
		return l.state, TriggerNone
	}

	const minSamples = 10
	n := float64(l.metrics.errors.RawValueCount())

	if n < minSamples {
		return OPEN, TriggerNone // Continue probing
	}

	rawErrorRate := l.metrics.errors.Mean()
	requiredSuccessRate := l.stated_slo.SuccessRate - (1-l.stated_slo.SuccessRate)*0.1

	// Use Adjusted Wald method to compute confidence interval for success rate
	// Using 2σ (95% confidence)
	const z = 2.0
	const z2 = z * z

	successCount := n * (1 - rawErrorRate)
	nAdj := n + z2
	pTilde := (successCount + z2/2) / nAdj
	se := math.Sqrt(pTilde * (1 - pTilde) / nAdj)

	lowerBound := pTilde - z*se // Success rate is at least this much or more
	upperBound := pTilde + z*se // Success rate is not more than this much

	if upperBound < requiredSuccessRate {
		return OPEN, TriggerRecoveryFailed
	}

	if lowerBound >= requiredSuccessRate {
		return CLOSED, TriggerRecoverySucceeded
	}

	// Not enough confidence yet, keep probing
	return OPEN, TriggerNone
}

func (l *Levee) mustOpen() (bool, Trigger) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.metrics.hasSufficientHistory() {
		return false, TriggerNone
	}

	n := float64(l.metrics.errors.RawValueCount())
	errorMean := l.metrics.errors.Mean()
	rawSuccessRate := 1 - errorMean

	// Adjusted Wald method for confidence interval
	// Using 3σ (99.7% confidence)
	const z = 3.0
	const z2 = z * z

	successCount := n * rawSuccessRate
	nAdj := n + z2
	pTilde := (successCount + z2/2) / nAdj
	se := math.Sqrt(pTilde * (1 - pTilde) / nAdj)

	upperBound := pTilde + z*se // Upper bound of success rate confidence interval

	srThreshold := l.stated_slo.SuccessRate - (1-l.stated_slo.SuccessRate)*0.1
	if upperBound < srThreshold {
		return true, TriggerSLOViolation
	}

	// Trend check: compare live Mean() to Base (last buffer wrap)
	currentLatency := l.metrics.latency.Mean()
	baseLatency := l.metrics.latency.Stat(Mean, Base)
	currentConcurrency := l.metrics.concurrency.Mean()
	baseConcurrency := l.metrics.concurrency.Stat(Mean, Base)

	// If both latency and concurrency are trending down, situation is improving
	if currentLatency < baseLatency && currentConcurrency < baseConcurrency {
		return false, TriggerNone
	}

	// Check against both time horizons
	midAnomaly := unexpectedLatencySpike(&l.metrics, Mid)
	longAnomaly := unexpectedLatencySpike(&l.metrics, Long)

	if midAnomaly || longAnomaly {
		return true, TriggerLatencyAnomaly
	}
	return false, TriggerNone
}

func unexpectedLatencySpike(m *metrics, horizon StatRange) bool {
	const epsilon = 1e-9

	currentLatency := m.latency.Mean()
	currentConcurrency := m.concurrency.Mean()
	historicalLatency := m.latency.Stat(Mean, horizon)
	historicalConcurrency := m.concurrency.Stat(Mean, horizon)
	historicalLatencyDev := m.latency.Stat(Deviation, horizon)

	// RPS calculation
	currentRPS := currentConcurrency / max(currentLatency, epsilon)
	historicalRPS := historicalConcurrency / max(historicalLatency, epsilon)

	// RPS multiplier: how much did traffic change?
	rpsX := currentRPS / max(historicalRPS, epsilon)

	// Expected latency multiplier: sub-linear scaling with load
	var expectedLatencyX float64
	if rpsX >= 1.0 {
		expectedLatencyX = 1.0 + math.Log(rpsX)
	} else {
		expectedLatencyX = 1.0 // No increase expected when load drops
	}

	// Actual latency multiplier
	actualLatencyX := currentLatency / max(historicalLatency, epsilon)

	// CV-based tolerance for natural variance
	cvLatency := historicalLatencyDev / max(historicalLatency, epsilon)
	tolerance := 1.0 + 3.0*cvLatency

	// Threshold: expected increase with variance tolerance
	threshold := expectedLatencyX * tolerance

	return actualLatencyX > threshold
}

// concurrencyErrorCorrelation estimates beta: error rate increase per unit concurrency
// Returns positive value when higher concurrency correlates with higher errors
func (m *metrics) concurrencyErrorCorrelation() float64 {
	const epsilon = 1e-9

	concurrencyMean := m.concurrency.Stat(Mean, Mid)
	concurrencyDev := m.concurrency.Stat(Deviation, Mid)
	errorMean := m.errors.Stat(Mean, Mid)
	errorDev := m.errors.Stat(Deviation, Mid)

	if concurrencyDev < epsilon || errorDev < epsilon {
		return 0.0
	}

	// Z-scores of current values
	currentConcurrency := m.concurrency.Mean()
	currentErrors := m.errors.Mean()
	concurrencyZ := (currentConcurrency - concurrencyMean) / concurrencyDev
	errorZ := (currentErrors - errorMean) / errorDev

	// Correlation coefficient (clamped to [-1, 1])
	rho := max(-1.0, min(1.0, concurrencyZ*errorZ))

	// Only return positive correlation if significant
	if rho > 0.3 {
		return rho * (errorDev / concurrencyDev)
	}
	return 0.0
}

// safeConcurrency calculates the maximum concurrency that keeps errors within SLO
func (l *Levee) safeConcurrency() float64 {
	beta := l.metrics.concurrencyErrorCorrelation()
	if beta < 1e-9 {
		return l.metrics.concurrency.Stat(Mean, Mid)
	}

	baseErrors := l.metrics.errors.Stat(Mean, Mid)
	baseConcurrency := l.metrics.concurrency.Stat(Mean, Mid)

	targetErrorRate := 1.0 - l.stated_slo.SuccessRate
	maxAllowedErrorRate := targetErrorRate * 0.9 // 10% margin from SLO

	allowableIncrease := maxAllowedErrorRate - baseErrors
	if allowableIncrease <= 0 {
		return l.safeConcurrencyFloor()
	}

	safe := baseConcurrency + (allowableIncrease / beta)
	return max(safe, l.safeConcurrencyFloor())
}

// safeConcurrencyFloor returns the minimum safe concurrency (half of long-term average)
func (l *Levee) safeConcurrencyFloor() float64 {
	return l.metrics.concurrency.Stat(Mean, Long) / 2.0
}

// shouldThrottle determines if we should enter THROTTLED state
func (l *Levee) shouldThrottle() (bool, float64) {
	if !l.metrics.hasSufficientHistory() {
		return false, 0
	}

	currentConcurrency := float64(l.Concurrents())
	safeConcurrency := l.safeConcurrency()

	// Only throttle if >10% above safe
	if currentConcurrency <= safeConcurrency*1.1 {
		return false, 0
	}

	// Only throttle if meaningful correlation exists
	if l.metrics.concurrencyErrorCorrelation() < 0.001 {
		return false, 0
	}

	// Don't throttle below floor
	if safeConcurrency < l.safeConcurrencyFloor() {
		return false, 0
	}

	// Too late if already near SLO boundary
	sloErrorRate := 1.0 - l.stated_slo.SuccessRate
	if l.metrics.errors.Mean() > sloErrorRate*0.9 {
		return false, 0
	}

	return true, safeConcurrency
}

// throttlingFailed determines if throttling has failed to prevent SLO violation
func (l *Levee) throttlingFailed() bool {
	// Error rate crossed SLO
	sloErrorRate := 1.0 - l.stated_slo.SuccessRate
	if l.metrics.errors.Mean() > sloErrorRate {
		return true
	}

	// Safe concurrency below floor
	if l.safeConcurrency() < l.safeConcurrencyFloor() {
		return true
	}

	// Base errors worse than Mid (situation degrading despite throttling)
	if l.metrics.errors.Stat(Mean, Base) > l.metrics.errors.Stat(Mean, Mid)*1.2 {
		return true
	}

	return false
}

// throttlingStabilised determines if we can exit THROTTLED and return to CLOSED
func (l *Levee) throttlingStabilised() bool {
	// Error rate dropped below 90% of Mid
	if l.metrics.errors.Mean() < l.metrics.errors.Stat(Mean, Mid)*0.9 {
		return true
	}

	// Safe concurrency exceeds Mid concurrency
	if l.safeConcurrency() > l.metrics.concurrency.Stat(Mean, Mid) {
		return true
	}

	return false
}
