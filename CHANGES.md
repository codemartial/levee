# Changelog

## v0.4.0

### Added

- Go diagnostic observability: state transitions now report a concrete trigger
  (statistical failure evidence, consecutive failures, proactive surge strain,
  excessive failures at the minimum limit, cooldown expiry, or healthy
  recovery). `Levee.Snapshot` exposes the current state, most recent trigger,
  inflight count, effective admission cap, capacity and error estimates, and
  proactive surge status without adding controller knobs or hot-path work. The
  zero trigger is now `TriggerNone` rather than `nil`; the field had previously
  been reserved with instructions not to depend on it. The README now presents
  `Call` as the canonical integration API.

- Proactive surge protection: a CLOSED (uncapped) breaker now trips to
  THROTTLED on congestion, before any failure evidence exists. The mechanism
  is a spring. While uncapped the breaker publishes a stretch onset -- an
  upper bound on the 1e-8 Poisson tail of healthy inflight around its
  Little's-law operating point (goodput x latency) -- and inflight beyond it
  loads a strain integral at a rate proportional to the fractional stretch.
  Proof of health relaxes it: completions prove capacity at completion rate
  x pre-surge latency (Little's law again), raising the base the stretch is
  measured against; a spent strain budget (one evaluation interval at unit
  stretch) trips, seeded at the proven capacity rather than the failure
  path's half-of-observed. Urgency scales continuously with spike size --
  time to trip is budget/stretch -- with no fixed confirmation window,
  latency multiplier, or hold timers. Capacity proven past the EWMA
  estimates persists as an excess decaying at the goodput half-life, whether
  the proof came from an armed window or from a throttled span that served
  its standing load at a healthy error rate, so recovery never re-trips on
  demand it has already demonstrated is servable. Strain survives a surge
  trip and relaxes only during calm uncapped time, so a re-flood right after
  recovery re-trips instantly. This closes the arrival-flood gap where queue
  saturation outruns the first outcome signal: in the mesh benchmark's
  overcap phase (8x load beyond max autoscaled capacity, 5s onset) the entry
  sheds within ~100ms of onset and never crashes, flipping the mesh result
  from -12251 to +8587 MeshDelta (best of field). The admission fast path
  stays lock-free at one extra atomic load (~4% on `BenchmarkThroughput`);
  the surge state fits in a 304-byte Levee struct. See EVOLUTION.md sections
  3.8 and 3.9.

### Engineering hardening

- Continuous integration on GitHub Actions: build, `go vet`, race-enabled tests,
  and coverage, run on both amd64 and arm64 to catch architecture-specific bugs.
- golangci-lint and govulncheck wired into CI; a nightly job fuzzes the admission
  and numeric paths.
- Native Go fuzz tests (`FuzzAdmission`, `FuzzInvNormCDF`, `FuzzWilson`),
  reference-value tests for the numeric core, and state-machine invariant tests.
- Full godoc on the exported API, runnable examples, and a `State` stringer.
- Contributor docs: CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, issue/PR templates,
  and a Makefile that mirrors the CI gate.

### Performance

- Added a lock-free admission fast path: while the breaker is healthy (uncapped,
  which can only hold while CLOSED), `Start` admits without taking the mutex, using
  an atomic `capped` gate plus atomic in-flight accounting. The footprint stays at
  248 bytes. This lifts contended throughput on a shared breaker by ~25-30% at high
  core counts. Completion (`Success`/`Fail`) is still serialized. See
  `BenchmarkContended`.

### Removed

- Removed `SaveState`, `RestoreState`, and `LeveeState` (breaking API change).
  State persistence was a holdover from when Levee tracked signals over multi-hour
  horizons; the EWMAs now settle within seconds, so a restarted breaker re-learns
  almost immediately. Dropping it also removes the corrupt-snapshot attack surface.
- Removed the `math.MaxFloat64` "uncapped" sentinel from the internals. The active
  limit is a plain float again, gated by an explicit `capped` flag, which makes the
  float-to-int admission overflow (the amd64 regression) structurally impossible.

### Fixes and tuning

- Fixed an amd64-only admission regression: converting the "unlimited" inflight
  cap (`math.MaxFloat64`) to int64 is implementation-defined and yields MinInt64
  on amd64, which made a healthy CLOSED breaker reject all traffic. The cast is
  now guarded, and `TestClosedUncapped` exercises it on both architectures.
- Reduced hard-coded magic in threshold derivation and improved handling of
  extreme SLOs and traffic.

## v0.3.0

This release made Levee fully self-contained and replaced the earlier
latency-anomaly / AIMD design with an error-rate-driven MIMD control law. (An
earlier draft of these notes described that superseded design; see EVOLUTION.md
for the full rationale of why it changed.)

### Architecture simplification

All circuit-breaker logic now lives directly in the `Levee` struct, behind a
single mutex, with no external dependencies. Removed: the `ICircuitBreaker`
interface, the separate `CircuitBreaker` and `WarmupCB` types, the `INIT` state
and explicit warmup phase, the `SLO.Warmup` field, and the standalone metrics
subsystem (ring buffers and trimmed-mean latency analysis).

### State model

Four states, driven by the error rate and inflight concurrency:

| State     | Behaviour                                                        |
| --------- | ---------------------------------------------------------------- |
| CLOSED    | Healthy; all traffic admitted uncapped.                          |
| THROTTLED | Adaptive inflight limit (MIMD) converging to observed capacity.  |
| OPEN      | All traffic rejected; cooldown backs off exponentially.          |
| HALF_OPEN | Post-OPEN probe at the minimum limit on an adaptive interval.    |

### Control logic

- Tripping (CLOSED -> THROTTLED): the Wilson lower confidence bound on the error
  EWMA (confidence = the SLO success rate) crossing the SLO error budget, or a
  run of consecutive failures (cold-start outage detection).
- THROTTLED (MIMD): the inflight limit is seeded from observed capacity
  (goodput x latency) and, each evaluation window, multiplied by sqrt(2) when the
  window error rate is within SLO or halved when above, floored at one inflight.
- Tripping to OPEN: error rate above 50% while already at the minimum limit. The
  cooldown is `SLO.Timeout`, backing off up to 16x across repeated trips.
- HALF_OPEN: after cooldown, one probe per (stretched) evaluation window at the
  minimum limit; recovers to CLOSED once the error EWMA falls below the recovery
  threshold (or capacity headroom is ample) past a recovery holdoff.
- Relaxing (CLOSED): an existing limit grows 2x per window and is removed
  entirely once observed capacity shows 3x headroom.

Evaluation windows are adaptive: roughly `clamp(avgLatency x 5, 100ms, 5s)`.

### Statistical core

- Error rate tracked as an EWMA with a 3-second half-life.
- Trip confidence derived from the SLO via the inverse normal CDF
  (`tripZ = invNormCDF(SuccessRate)`), applied as a Wilson score lower bound.
- Capacity estimated from goodput and average-latency EWMAs.

### Persistence and reporting

- `SaveState()` / `RestoreState()` snapshot the learned signals so a restart
  resumes warm; the live inflight count is intentionally dropped on restore.
- Every admission and completion call returns a `StateChange` carrying the
  resulting `State`. Its `Trigger` field is reserved for future use (nil today).

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
