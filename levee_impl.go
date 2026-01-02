package levee

import (
	"math"
	"math/rand/v2"
)

// probingAllowed checks if a call should be allowed during OPEN probing phase.
// Rate-limits to ~10% of historical throughput, scaling with error rate.
func (l *Levee) probingAllowed() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	const minSamples = 10

	// Always use historical EWMA for concurrency (preserved across cooldown)
	hConcurrency := l.metrics.concurrency.Stat(Mean, Mid)
	if hConcurrency <= 0 {
		return l.concurrents <= 1
	}

	// Floor at 10% of historical concurrency, capped at 1
	// This rate-limits probing to ~10% of baseline throughput fleet-wide
	floor := min(1.0, 0.1*hConcurrency)

	// Get error rate: fresh data if available, otherwise use floor only
	var hErrors float64
	if l.metrics.errors.isFilled {
		hErrors = l.metrics.errors.Stat(Mean, Mid)
	} else if l.metrics.errors.RawValueCount() >= minSamples {
		hErrors = l.metrics.errors.Mean()
	} else {
		// Not enough samples yet, probe at floor rate
		return l.allowCall(floor)
	}

	// Scale by success rate, but never below floor
	allowedConcurrency := max((1-hErrors)*hConcurrency, floor)
	return l.allowCall(allowedConcurrency)
}

// allowCall checks if current concurrency is within allowed limit.
// Uses probabilistic admission when allowedConcurrency < 1.
func (l *Levee) allowCall(allowedConcurrency float64) bool {
	if allowedConcurrency >= 1 {
		return float64(l.concurrents) <= allowedConcurrency
	}
	// Below 1: at most 1 concurrent, probabilistically allowed
	if l.concurrents > 1 {
		return false
	}
	return rand.Float64() < allowedConcurrency
}

// newState evaluates if recovery has succeeded or failed during OPEN probing.
func (l *Levee) newState() (State, Trigger) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.state != OPEN || !l.cooldownComplete {
		return l.state, TriggerNone
	}

	const minSamples = 10
	n := float64(l.metrics.errors.RawValueCount())
	if n < minSamples {
		return OPEN, TriggerNone
	}

	rawErrorRate := l.metrics.errors.Mean()
	requiredSuccessRate := l.slo.SuccessRate - (1-l.slo.SuccessRate)*0.1

	successCI := waldConfidenceInterval(n, 1-rawErrorRate, 2.0)
	if successCI.upper < requiredSuccessRate {
		return OPEN, TriggerRecoveryFailed
	}
	if successCI.lower >= requiredSuccessRate {
		return CLOSED, TriggerRecoverySucceeded
	}

	return OPEN, TriggerNone
}

// mustOpen checks if circuit should open due to SLO violation.
func (l *Levee) mustOpen() (bool, Trigger) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.metrics.hasSufficientHistory() {
		return false, TriggerNone
	}

	n := float64(l.metrics.errors.RawValueCount())
	rawSuccessRate := 1 - l.metrics.errors.Mean()

	successCI := waldConfidenceInterval(n, rawSuccessRate, 3.0)
	srThreshold := l.slo.SuccessRate - (1-l.slo.SuccessRate)*0.1

	if successCI.upper < srThreshold {
		return true, TriggerSLOViolation
	}
	return false, TriggerNone
}

// confidenceInterval represents a statistical confidence interval
type confidenceInterval struct {
	lower, upper float64
}

// waldConfidenceInterval computes Adjusted Wald confidence interval for a proportion
func waldConfidenceInterval(n, p, z float64) confidenceInterval {
	z2 := z * z
	nAdj := n + z2
	pTilde := (n*p + z2/2) / nAdj
	se := math.Sqrt(pTilde * (1 - pTilde) / nAdj)
	return confidenceInterval{
		lower: pTilde - z*se,
		upper: pTilde + z*se,
	}
}

// hasLatencyAnomaly checks if there's a latency spike that warrants throttling.
// Returns (hasAnomaly, currentConcurrency) where currentConcurrency becomes the throttle ceiling.
func (l *Levee) hasLatencyAnomaly() (bool, float64) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if !l.metrics.hasSufficientHistory() {
		return false, 0
	}

	currentLatency := l.metrics.latency.Mean()
	baseLatency := l.metrics.latency.Stat(Mean, Base)
	currentConcurrency := l.metrics.concurrency.Mean()
	baseConcurrency := l.metrics.concurrency.Stat(Mean, Base)

	// Situation is improving - no anomaly
	if currentLatency < baseLatency && currentConcurrency < baseConcurrency {
		return false, 0
	}

	if l.unexpectedLatencySpike(Mid) || l.unexpectedLatencySpike(Long) {
		return true, currentConcurrency
	}
	return false, 0
}

func (l *Levee) unexpectedLatencySpike(horizon StatRange) bool {
	const epsilon = 1e-9

	currentLatency := l.metrics.latency.Mean()
	currentConcurrency := l.metrics.concurrency.Mean()
	historicalLatency := l.metrics.latency.Stat(Mean, horizon)
	historicalConcurrency := l.metrics.concurrency.Stat(Mean, horizon)
	historicalLatencyDev := l.metrics.latency.Stat(Deviation, horizon)

	currentRPS := currentConcurrency / max(currentLatency, epsilon)
	historicalRPS := historicalConcurrency / max(historicalLatency, epsilon)
	rpsMultiplier := currentRPS / max(historicalRPS, epsilon)

	// Expected latency increase: sub-linear scaling with load
	expectedLatencyMultiplier := 1.0
	if rpsMultiplier >= 1.0 {
		expectedLatencyMultiplier = 1.0 + math.Log(rpsMultiplier)
	}

	actualLatencyMultiplier := currentLatency / max(historicalLatency, epsilon)

	// CV-based tolerance for natural variance
	cv := historicalLatencyDev / max(historicalLatency, epsilon)
	threshold := expectedLatencyMultiplier * (1.0 + 3.0*cv)

	return actualLatencyMultiplier > threshold
}

// throttlingStabilised checks if error rate has improved enough to exit THROTTLED.
func (l *Levee) throttlingStabilised() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	currentErrors := l.metrics.errors.Mean()
	longTermErrors := l.metrics.errors.Stat(Mean, Long)
	errorDev := l.metrics.errors.Stat(Deviation, Long)

	// Exit when errors drop to long-term baseline + 2σ
	return currentErrors < longTermErrors+2.0*errorDev
}
