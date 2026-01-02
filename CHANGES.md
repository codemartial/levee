# Levee v0.3.0

## Architecture Simplification

**Removed:**
- `ICircuitBreaker` interface
- `CircuitBreaker` struct (separate from Levee)
- `WarmupCB` struct and warmup phase
- `INIT` and `HALF_OPEN` states
- `SLO.Warmup` field
- `revised_slo` field (only `slo` remains)

**Levee is now self-contained** - all circuit breaker logic lives directly in `Levee` struct.

---

## State Model Changes

| Trunk | v0.3.0 |
|-------|--------|
| INIT → CLOSED → OPEN → HALF_OPEN → CLOSED | CLOSED ↔ OPEN ↔ THROTTLED |

**OPEN state** now has two phases:
1. **Cooldown**: Zero traffic until timeout expires
2. **Probing**: Rate-limited calls via `probingAllowed()`

**THROTTLED state** (new): AIMD-based concurrency control triggered by latency anomalies before SLO breach.

---

## Probing Logic (`probingAllowed()`)

```go
hConcurrency := l.metrics.concurrency.Stat(Mean, Mid)  // Historical EWMA
floor := min(1.0, 0.1*hConcurrency)                    // 10% of baseline, capped at 1
allowedConcurrency := max((1-hErrors)*hConcurrency, floor)
```

**Key features:**
- Uses historical EWMA for concurrency (preserved across cooldown)
- Fresh error data from probing phase (raw Mean if < 100 samples)
- Probabilistic admission when `allowedConcurrency < 1`
- Fleet-friendly: ~10% of baseline throughput fleet-wide

---

## THROTTLED State (New)

Triggered by latency anomaly detection (not SLO breach):
- **Entry**: `hasLatencyAnomaly()` detects unexpected latency spike
- **AIMD control**: Every 50 samples, adjust ceiling (1.1x up / 0.5x down)
- **Exit**: `throttlingStabilised()` when errors drop to baseline + 2σ
- **Floor**: Long-term average concurrency

---

## Other Changes

**stats.go:**
- Added dynamic buffer sizing infrastructure (constants, fields)
- `RecordAt()` now takes timestamp parameter
- Added `RecordConcurrency()` with timestamp

**state.go:**
- Simplified `SaveState()`/`RestoreState()` - no more `CircuitBreaker` type assertions
- Direct access to `l.metrics` instead of `cb.metrics`

**Atomic concurrency control:**
- `throttleConcurrency` is `atomic.Uint64` (float64 bits)
- Helper methods: `loadThrottleConcurrency()`, `storeThrottleConcurrency()`

---

## Trigger Constants

| Removed | Added |
|---------|-------|
| `TriggerWarmupComplete` | `TriggerLatencyAnomaly` |
| | `TriggerThrottlingStabilised` |

---

# Levee v0.2.0

A major update to the self-tuning circuit breaker, featuring smarter decision-making, flexible integration options, and production-grade benchmarking.

## Highlights

### 2x Better Decision-Making Than Static Circuit Breakers

Levee now outperforms meticulously tuned static circuit breakers by over 2x in overall decision quality. In head-to-head benchmarks against a 28-hour Cyber Monday traffic simulation:

- Up to 10x faster incident detection - Levee opens the circuit before bad traffic overwhelms your backend
- Up to 1.7x less unnecessary blocking - Levee recovers quickly and avoids rejecting good traffic

Static circuit breakers force you to choose between "tuned for normal traffic" (which flaps during spikes) or "tuned for peak traffic" (which is slow to react). Levee adapts continuously so you don't have to choose.

### Out-of-Band Operation

You can now integrate Levee into your code without wrapping calls:

```go
// Check before starting work
stateChange, err := l.Start(time.Now())
if err != nil {
    return // Circuit is open
}

// Do the work
result := callBackend()

// Report the outcome
if result.Err != nil {
    l.Fail(time.Now(), duration)
} else {
    l.Success(time.Now(), duration)
}
```

This enables integration with stream processors, event-driven architectures, and anywhere you need explicit timing control.

### State Persistence

Circuit breaker state can now be saved and restored across restarts:

```go
state := cb.SaveState()
// ... persist to disk or cache ...
cb.RestoreState(state)
```

No more cold-start problems where a restarting service floods a degraded backend.

### Clock-Independent Operation

All methods now accept explicit timestamps instead of using wall-clock time. This enables:

- Deterministic testing and replay
- Event-time processing for stream applications
- Simulation-based validation

## Technical Improvements

### Statistical Forecasting

Levee now uses Adjusted Wald confidence intervals for success rate estimation, providing statistically rigorous OPEN/CLOSED decisions even with limited sample sizes. This replaces simple threshold-based logic that was prone to noise.

### Latency Spike Detection

New RPS-aware latency analysis detects unexpected latency spikes using coefficient of variation, providing early warning before error rates climb.

### Ring Buffer Metrics

Switched from time-based windowing to fixed-size ring buffers, ensuring predictable memory usage regardless of traffic patterns.

## Benchmark Suite

A new comprehensive benchmark framework compares circuit breakers against a "Prescient Breaker" - an oracle that knows the future and makes perfect decisions. This provides ground truth for evaluating real-world CB performance.

The Cyber Monday simulation covers:
- 28 hours of realistic e-commerce traffic patterns
- Multiple incident types: traffic spikes, retry storms, gradual degradation
- Traffic ranging from 100 RPS baseline to 48x peak load

Run with: `go test -bench=BenchmarkCyberMondayPrescient -benchtime=1x -v`

## Breaking Changes

- `Call()` now returns `StateChange` instead of `State`
- All timing methods require explicit `time.Time` parameter

## Migration

Replace:
```go
state, err := cb.Call(fn)
```

With:
```go
stateChange, err := cb.Call(fn)
state := stateChange.State
```
