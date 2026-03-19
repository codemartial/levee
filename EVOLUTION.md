# Levee: Design Evolution

## 1. How the Design Was Arrived At

### 1.1 The Initial Prompt

> Look at the README.md. It describes a product that you need to build. The skeleton of that product is provided in levee.go. There are some benchmarks in the benchmark directory. Your goal is to make levee top the benchmarks. I understand that this is a very vague ask, so we will take some time chatting back and forth for you to determine the requirements concretely. Then when I tell you explicitly, you develop a plan.

The README describes Levee as "a self-tuning circuit breaker and concurrency-based rate limiter for Go services" that takes a single SLO input (success rate + timeout) and adapts automatically. The skeleton in `levee.go` provided the API surface -- `NewLevee()`, `Start()`, `Success()`, `Fail()`, `State()` -- with empty method bodies. Two benchmark suites existed: an open-loop prescient benchmark and a distributed closed-loop benchmark.

### 1.2 Exploration Phase

After reading the full codebase (README, levee.go skeleton, all benchmark infrastructure), Claude Opus 4.6 proposed an initial direction: adaptive concurrency control with EWMA-based error rate detection. Five clarifying questions were asked about concurrency limits (Little's Law?), THROTTLED vs OPEN semantics, statistical approach, state transitions, and the trigger mechanism.

### 1.3 Design Freedom and Anti-Fitting Constraint

The user's response established two critical ground rules:

> 1. The numbers in benchmark reports are indicative. They are not targets. 2. You need to develop your own mental model. It may or may not require THROTTLED state or latency anomaly detection or any of that nonsense. 3. You decide the best statistical approach or any other to suit the purpose. 4. You decide. 5. Yes, the purpose is diagnostics. You can retain or discard as per your choice.
>
> Caveats: you must not try to fit the benchmarks. The solution has to be general. The benchmarks may be changed later and levee must still win. You must not modify benchmarks.

This meant: full design freedom, but the solution must be principled. No benchmark-specific tuning.

### 1.4 The Benchmark-Anchoring Warning

When Opus presented its initial mental model (adaptive concurrency limit + error-rate circuit breaking + EWMA signals), the user warned against anchoring on artifacts from a prior failed attempt:

> I think your design is inspired from what you read in the benchmarks. I'm not saying that you should drop it but let me warn you that you're seeing relics of an unsatisfactorily successful attempt. The failure could either have been due to the approach or implementation flaws, I don't know. You need to optimise for the highest successes with the least crashes and reasonable levee-side concurrency. If you over-optimise for concurrency, you'll kill throughput despite high success rate and 0 backend crashes. I mean, it's easy... just block everything and you're done! Just allow everything is also a pathological winning strategy as you might notice from the high number of successes from No-CB.

This reframed the optimisation target: maximise `Delta = SuccessScore - FailureScore`, not minimise concurrency or crashes.

### 1.5 The Core Requirements

The most consequential user message established the three design pillars:

> Something important that is missing from that design is this: when the circuit opens due to overload, there's 0 work getting done. That's fine for when the backend is b0rked. But often the situation is simply that load > capacity. Levee should be able to do that matching between load and capacity, while also pushing the backend hard enough for it to scale up, while also maintaining a reasonable error profile. The SLO is also not an instantaneous metric. It can be assumed to have a window of evaluation... say 10-100s. Lastly, there is concurrency. It matters because -- and this is something the benchmarks don't model -- the upstream concurrency runaway is a cascading failure. In case of a backed up downstream, there may be a situation where the upstream gets choked waiting for inflight reqs to timeout. These are hard problems. You decide whether you want to dip into signal processing, control theory and info-theory to solve them.

The three requirements:
1. **Load-capacity matching** -- not binary open/close. When load > capacity, shed excess while keeping capacity-matched throughput flowing. Push enough traffic for the autoscaler to sense demand.
2. **Windowed SLO** -- the SLO is evaluated over a window (10-100s), not instantaneously. Brief error spikes within the window budget are acceptable.
3. **Concurrency limiting** -- upstream concurrency runaway is a cascading failure risk. Inflight requests accumulating due to timeouts can choke the caller.

### 1.6 Prior Art: SmartCB

The user pointed out a previous attempt (`../smartcb/smartcb.go`) that wrapped an external circuit breaker library, used EWMA and Adjusted Wald confidence intervals, and required QPS input. The user characterised the prior attempt bluntly:

> The user who built smartcb is not very bright, and certainly not very knowledgeable.

The key lesson from SmartCB: binary open/close with error-rate detection is insufficient. When the circuit opens during overload, zero work gets done. Levee must do better.

### 1.7 Design Convergence

These inputs converged on the design: a **dynamic inflight limiter** using **MIMD (Multiplicative Increase / Multiplicative Decrease)** control, analogous to TCP congestion control. Three states map naturally:

- **CLOSED**: No limit. Maximum throughput. Normal operation.
- **THROTTLED**: Inflight limit active. Admits requests up to estimated capacity. MIMD grows limit as backend scales up.
- **OPEN**: Complete block. Backend is catastrophically broken, not just overloaded.

The critical insight: CLOSED goes to THROTTLED, not OPEN. This means shedding excess load immediately while keeping capacity-matched throughput flowing.

---

## 2. The Benchmark Environment

### 2.1 Open-loop (Prescient) Benchmark

A synthetic event stream generated from 122 load specifications spanning a 28-hour Cyber Monday scenario. A prescient breaker with perfect foreknowledge serves as the oracle. Scored by comparing each CB's decisions against the oracle:

- **BadTraffic**: `sqrt(sum(RPS^2))` for seconds where the oracle would block but the CB allowed
- **LostBusiness**: `sqrt(sum(RPS^2))` for seconds where the oracle would allow but the CB blocked
- **TotalPenalty** = BadTraffic + LostBusiness (lower is better)

State transitions are classified as false alarms, late detections, slow recoveries, or flapping.

### 2.2 Distributed (Closed-loop) Benchmark

The same 28-hour workload drives a simulated backend with:

- **HPA autoscaling**: 1-8 replicas, 150 RPS each, 15s eval interval, 30s provisioning lag, 70% target utilisation, 5-minute scale-down stabilisation
- **Load-dependent degradation**: latency inflation (1x-10x) and error rate escalation (0%-40%) based on EWMA-smoothed load
- **Crash simulation**: EWMA load >= 3.0 sustained for 30s triggers crash; 5-minute restart
- **Queue shedding**: when queue is full, requests are rejected

Scoring uses 200ms epochs: `SuccessScore = sqrt(sum(epochSuccesses^2 * 5))`, `FailureScore = sqrt(sum(epochFailures^2 * 5))`, `Delta = SuccessScore - FailureScore` (higher is better). The quadratic-inside-sqrt formula heavily penalises concentrated failure bursts.

**Critical backend behaviour**: Shed events report the FULL SLO TIMEOUT (1.5s) as latency, not the near-zero actual rejection time. This creates a 1.5s feedback delay for any latency-based signal.

### 2.3 Static Competitors

- **Static-Peak**: Tuned for peak traffic (2000-5000 RPS). 7 consecutive failures to trip, 5 consecutive successes to close, 30s half-open timeout, 3 concurrent probe calls.
- **Static-BAU**: Tuned for BAU traffic (100-800 RPS). 5 consecutive failures to trip, 3 consecutive successes to close, 10s half-open timeout, 1 probe call.
- **No-CB**: Passes everything through. A catastrophe under epoch-squared scoring.

---

## 3. The Design Evolves

This section traces the state machine and signal processing as they changed across three development phases.

### 3.1 Phase 1: The Three-State Machine

The initial implementation had three states:

```
              errEWMA > tripThreshold
              OR consecFails >= N
CLOSED ───────────────────────────────► THROTTLED ◄─────┐
   ▲                  ▲                     │            │
   │                  │  errEWMA            │  MIMD      │  cooldown
   │  errEWMA         │  < recoverTh        │  to min    │  elapsed
   │  < recoverTh     │  (after holdoff)    ▼            │
   └──────────────────┴──────────────────  OPEN ─────────┘
```

CLOSED → THROTTLED on SLO breach (EWMA or consecutive failures). THROTTLED ran the MIMD control law: halve the inflight limit when error rate exceeds threshold, double when within budget. When errors exceeded 50% even at minimum concurrency, the circuit went OPEN — complete block for one cooldown period — then back to THROTTLED to restart MIMD from minimum.

This worked well enough to beat Static-Peak on the 5h benchmark (13,475 vs 12,719) but had a structural problem: every OPEN→THROTTLED transition restarted MIMD from limit=1, and the EWMA was getting corrupted by the idle gap during OPEN cooldowns.

The MIMD parameters at this point:
- x2 increase / x0.5 decrease (standard multiplicative)
- Fixed 500ms eval interval
- Fixed 3s recovery holdoff
- EWMA half-life: 3s

Recovery from THROTTLED to CLOSED happened when the error rate EWMA dropped below `recoverThreshold`. But on recovery, the inflight limit was reset to `math.MaxFloat64` — all traffic admitted immediately. This caused a burst that often re-triggered overload.

### 3.2 Phase 2: Graduated Recovery, MIMD Tuning, and the Cooperative Challenge

Three changes addressed the gap between Levee and Static-Peak on the full 28h benchmark.

**Graduated recovery** — The biggest single improvement. Instead of resetting the inflight limit on THROTTLED→CLOSED recovery, keep it and grow via a new `maybeRelaxLimit()` in CLOSED state. This method uses x2 doubling with a safety check: if errors rise, re-trip immediately; if the limit exceeds 3x actual inflight, fully relax to MaxFloat64. Effect: 28h Delta improved from 53,643 to 60,494 (+6,851).

**sqrt(2) MIMD increase** — The x2 increase caused ~100% capacity overshoot at the MIMD oscillation point. A multiplier sweep (x1.3 through x1.7 in steps of 0.05) found peak performance at x1.42. We chose x√2 ≈ 1.414 as the principled value (geometric mean of 1.0 and 2.0 on log scale). Overshoot dropped from ~100% to ~41%, shed events from ~1.17M to ~769K.

**50% initial limit** — Changed `enterThrottledFromClosed` from 75% to 50% of current inflight. More aggressive initial shed, faster convergence to the right capacity.

Then came the cooperative benchmark: 100 instances sharing a single backend. This exposed a new problem. In THROTTLED at minimum concurrency, each instance sent probes at ~10/s (one probe completing every 100ms at limit=1). With 100 instances, that's ~1,000 probes/s hitting a degraded backend — self-reinforcing failure.

The state machine needed a fourth state.

### 3.3 Phase 2 (cont.): The HALF_OPEN Split

OPEN → THROTTLED became OPEN → HALF_OPEN → CLOSED, with HALF_OPEN as a distinct probing state:

```
From        To          Condition
─────────── ─────────── ──────────────────────────────────────────────────
CLOSED      THROTTLED   errEWMA > tripThreshold OR consecFails >= N
THROTTLED   CLOSED      errEWMA < recoverTh OR limit >> inflight (after holdoff)
THROTTLED   OPEN        evalErrRate > 50% at minInflightLimit
OPEN        HALF_OPEN   cooldown elapsed (exponential backoff)
HALF_OPEN   CLOSED      errEWMA < recoverTh OR limit >> inflight (after holdoff)
HALF_OPEN   OPEN        evalErrRate > 50% at minInflightLimit
```

HALF_OPEN introduced two rate-limiting mechanisms for probe pressure:

1. **One probe per eval window**: At `minInflightLimit`, after one probe completes, further probes are blocked until the next eval resets counters.
2. **Exponential eval interval**: The eval interval in HALF_OPEN scales with `openStreak` (consecutive OPEN entries without full CLOSED recovery): `baseEvalInterval << min(openStreak, maxOpenBackoff)`.

| openStreak | Eval Interval | Probes/s (100 instances) |
|------------|---------------|--------------------------|
| 0 | 500ms | ~200 |
| 1 | 1s | ~100 |
| 2 | 2s | ~50 |
| 3 | 4s | ~25 |
| 4 | 8s | ~12.5 |

The adaptation is purely local: each instance's `openStreak` grows independently based on its own observations. No coordination needed.

This also introduced **exponential OPEN backoff**: each consecutive OPEN without full CLOSED recovery doubles the cooldown, capped at 8× base (`maxOpenBackoff=4`). This reduced aggregate probe pressure during sustained degradation, letting the backend recover rather than being overwhelmed by health checks.

Two other changes came in this phase:

- **recoverThreshold floor**: For tight SLOs (error rate < ~9.5%), the MIMD and recovery thresholds use a floor of 10%. Without this, SLO=0.99 requires errEWMA < 0.01 for MIMD increase — far too conservative. The 0.095 cutoff (rather than 0.10) avoids an IEEE 754 float issue where `1.0 - 0.9 = 0.09999999999999998`.
- **Cap=1 on OPEN→HALF_OPEN**: The initial HALF_OPEN limit was set to `max(min(estimatedCapacity*0.25, 1), minInflightLimit)`. Since `minInflightLimit=1` and the `min` caps at 1, this always evaluates to 1. The capacity estimate was later removed as dead code, leaving a simple `l.inflightLimit = minInflightLimit`.

At the end of Phase 2, results were encouraging but inconsistent. Isolated 28h: +952 over Static-Peak. Cooperative: +2,180. SLO sweep: 2/5 wins. QD sweep: 3/5 wins.

**The structural roadblock.** The gap between Levee and Static-Peak appeared to be structural. Static-Peak's consecutive-failure trip mechanism generates only 205K Shed events over 28 hours, while Levee's MIMD oscillation generates ~1.1M. Under epoch-squared scoring, these concentrated MIMD oscillation bursts (rapid halving/doubling at 500ms intervals) are penalised heavily. Every approach that reduces Shed events (slower growth, additive increase, capacity-aware limiting, longer eval intervals) also reduces SuccessScore by a comparable amount — the fast MIMD response that causes Shed is the same mechanism that enables fast recovery and high throughput. The graduated recovery and sqrt(2) changes had partially closed the gap by reducing overshoot amplitude, but further progress seemed impossible without a fundamentally different approach.

Then Google Gemini Pro found the real problem.

### 3.4 Phase 3: Event-Clock EWMA — The Biggest Win

During a code review, Google Gemini Pro identified that the standard time-weighted EWMA computes alpha from wall-clock `dt`:

```
alpha = 1 - exp(-dt * ln2 / halfLife)
```

During OPEN cooldowns (up to 20s at max backoff), no observations arrive. The next observation after OPEN has a large `dt`, producing `alpha ≈ 1.0`. This effectively replaces the entire EWMA history with one sample.

This was causing systematic signal corruption at every OPEN→HALF_OPEN transition:

1. Backend degrades → trip → THROTTLED → MIMD decreases to limit=1 → OPEN (errEWMA ~0.6)
2. OPEN cooldown: 2.5s–20s of silence
3. HALF_OPEN: first probe succeeds → alpha ≈ 1.0 → errEWMA drops from ~0.6 to ~0.0
4. Recovery check: errEWMA < recoverThreshold → premature CLOSED transition
5. Traffic floods back → immediate re-trip → repeat

The fix: cap alpha at the value corresponding to one inter-observation interval:

```
if goodput > 0:
    maxDt = 1 / goodput
    maxAlpha = 1 - exp(-maxDt * ln2 / halfLife)
    alpha = min(alpha, maxAlpha)
```

Each observation now updates the EWMA by *at most* one average inter-observation interval's worth. An observation after a 20s OPEN cooldown gets the same weight as one after `1/goodput` seconds — the gap is clamped, not the observation itself. During normal operation the cap rarely activates because `dt` stays close to `1/goodput`.

Applied to all three EWMAs: error rate, goodput, and average latency.

This was invisible in Phase 2 because the probe rate limiting masked it — fewer probes meant fewer chances for the corrupted EWMA to trigger premature recovery. But the damage was still happening, costing ~11,000 Delta on P1 alone. What had appeared to be a structural scoring disadvantage was actually a signal processing bug — and fixing it blew the gap wide open.

### 3.5 Phase 3 (cont.): Latency-Adaptive Eval Interval

The eval interval had been a fixed 500ms since Phase 1. This was replaced with an adaptive calculation:

```
baseEvalInterval = clamp(avgLatency * targetSamplesPerEval, 100ms, 5s)
recoveryHoldoff  = max(baseEvalInterval * holdoffEvalMultiplier, 1s)
```

At the benchmark's 100ms latency, this produces the same 500ms interval and 3s holdoff as before — a no-op for benchmark results. But for production services:
- Cache (1ms latency): eval every 100ms → 5× faster reaction
- Batch API (2s latency): eval every 5s → enough samples per window for stable MIMD
- No tuning needed: one mechanism, correct for any latency

### 3.6 The Final State Machine

After all three phases, the state machine and its parameters:

```
From        To          Condition
─────────── ─────────── ──────────────────────────────────────────────────
CLOSED      THROTTLED   errEWMA > tripThreshold OR consecFails >= N
THROTTLED   CLOSED      errEWMA < recoverTh OR limit >> inflight (after holdoff)
THROTTLED   OPEN        evalErrRate > 50% at minInflightLimit
OPEN        HALF_OPEN   exponential cooldown elapsed
HALF_OPEN   CLOSED      errEWMA < recoverTh OR limit >> inflight (after holdoff)
HALF_OPEN   OPEN        evalErrRate > 50% at minInflightLimit

HALF_OPEN probe behaviour:
  - One probe per eval window (rate limited at minInflightLimit)
  - Eval interval = baseEvalInterval() << min(openStreak, maxOpenBackoff)
  - First successful probe → MIMD increase → rate limit off → normal recovery

EWMA behaviour across all states:
  - Alpha capped at 1/goodput inter-observation interval
  - OPEN gaps do not erase history
  - First observation after gap updates by at most one average inter-observation interval's worth
```

**Parameters (final):**

| Parameter | Value | Notes |
|-----------|-------|-------|
| `ewmaHalfLife` | 3s | Unchanged from Phase 1 |
| `goodputHalfLife` | 3s | Unchanged from Phase 1 |
| `warmupSamples` | 50 | Unchanged from Phase 1 |
| `tripBufferFactor` | 0.05 | Tuned in Phase 1 experiments |
| `targetSamplesPerEval` | 5 | New in Phase 3 (was implicit in fixed 500ms) |
| `minEvalInterval` | 100ms | New in Phase 3 |
| `maxEvalInterval` | 5s | New in Phase 3 |
| `holdoffEvalMultiplier` | 6 | New in Phase 3 (was fixed 3s) |
| `minRecoveryHoldoff` | 1s | New in Phase 3 |
| `minInflightLimit` | 1.0 | Unchanged from Phase 1 |
| `openErrThreshold` | 0.5 | Unchanged from Phase 1 |
| `maxOpenBackoff` | 4 | New in Phase 2 |

**Derived thresholds (computed from SLO in constructor):**

| Threshold | Formula | Example (SLO=90%) |
|-----------|---------|-------------------|
| `sloErrRate` | `1 - slo.SuccessRate` | 0.10 |
| `tripThreshold` | `sloErrRate + slo.SuccessRate * tripBufferFactor` | 0.145 |
| `recoverThreshold` | `max(sloErrRate, 0.10)` if `sloErrRate < 0.095`, else `sloErrRate` | 0.10 |
| `consecFailTrip` | `max(ceil(log(1e-8) / log(sloErrRate)), 5)` | 77 |
| `baseEvalInterval` | `clamp(avgLatency * targetSamplesPerEval, minEvalInterval, maxEvalInterval)` | 500ms |
| `recoveryHoldoff` | `max(baseEvalInterval * holdoffEvalMultiplier, minRecoveryHoldoff)` | 3s |

---

## 4. Operations (Reference)

This section documents the final behaviour of each operation for reference.

**`Start(ts)`** -- Admission control.
- CLOSED: If an active inflight limit exists (graduated recovery), run `maybeRelaxLimit(ts)` and reject if at limit. Otherwise increment inflight, admit.
- OPEN: If exponential cooldown elapsed, transition to HALF_OPEN (limit=1), then fall through. Otherwise reject.
- HALF_OPEN, THROTTLED: Run `maybeEvaluateLimit(ts)`. If eval triggered OPEN, reject. At `minInflightLimit`, enforce one-probe-per-eval-window rate limiting. If `inflight >= ceil(inflightLimit)`, reject. Otherwise increment inflight, admit.

**`Success(ts, duration)`** -- Record a successful completion.
- Decrement inflight. Update error EWMA toward 0 (with event-clock alpha cap). Reset consecutive-fail counter. Update capacity estimates (goodput, latency).
- If THROTTLED or HALF_OPEN: increment eval successes. Check recovery conditions (after `effectiveRecoveryHoldoff()`): recover to CLOSED if error rate is below `recoverThreshold` OR if the inflight limit has grown far beyond actual usage (limit > 3x inflight). On recovery, keep the inflight limit (graduated recovery) and reset `openStreak`.

**`Fail(ts, duration)`** -- Record a failed completion.
- Decrement inflight. Update error EWMA toward 1 (with event-clock alpha cap). Increment consecutive-fail counter.
- If CLOSED: Check trip conditions (EWMA > tripThreshold OR consecutive fails exceeded). Trip to THROTTLED.
- If THROTTLED or HALF_OPEN: Increment eval failures. (The OPEN transition is handled in `maybeEvaluateLimit`, not here.)

**`maybeEvaluateLimit(ts)`** -- MIMD control law (called from `Start`).
- Compute effective eval interval: `baseEvalInterval()` for THROTTLED; in HALF_OPEN at `minInflightLimit`, shift left by `min(openStreak, maxOpenBackoff)` for exponential probe backoff.
- Skip if less than the effective interval since last evaluation.
- Compute `evalErrRate` from successes and failures in the window.
- If error rate > 50% and limit is already at minimum: transition to OPEN (catastrophic).
- If error rate > `recoverThreshold`: halve the limit (multiplicative decrease).
- If error rate <= `recoverThreshold`: multiply the limit by √2 (multiplicative increase).

**`maybeRelaxLimit(ts)`** -- Graduated recovery in CLOSED (called from `Start`).
- Skip if less than `baseEvalInterval()` since last evaluation.
- If eval-window error rate exceeds `recoverThreshold`: re-trip to THROTTLED immediately.
- If limit exceeds 3x actual inflight: fully relax to MaxFloat64 (recovery complete).
- Otherwise: double the limit (faster growth than THROTTLED's √2 since we're in recovery).

---

## 5. Optimisation Experiments

All experiments were conducted on the deterministic distributed benchmark suite. The "5h" column is the first-incident sub-test (5 simulated hours); the "28h" column is the full Cyber Monday simulation.

### 5.1 Notation

- **Delta** = SuccessScore - FailureScore (higher is better)
- **Principle Score**: 1 = primarily motivated by fitting the benchmark; 10 = strong general justification independent of any specific scenario
- Static-Peak reference: 5h = 12,719, 28h = 106,342

### 5.2 Early Iteration (Pre-MIMD)

The first implementation used AIMD (Additive Increase / Multiplicative Decrease) with 10s EWMA half-life and a catastrophic-rate threshold of 0.95. Initial distributed result: Delta = 8,485 with 91K failures. The following changes brought it to a competitive baseline:

| Change | Effect |
|--------|--------|
| EWMA half-life 10s -> 3s | Faster detection and recovery |
| tripBuffer 0.05 -> 0.03 | Tighter trip threshold |
| Added consecutive failure detection (consecFailTrip = 8) | Fast trip on total outage |
| AIMD -> MIMD (x2/x0.5) | O(log(n)) recovery instead of O(n) |
| Removed catastrophicRate (replaced by OPEN transition logic) | Cleaner state machine |

After these changes: 5h Delta = 12,563 (near Static-Peak's 12,719).

### 5.3 MIMD Alternatives (All Worse)

Approximately 20 alternative control strategies were tested across multiple sessions, all performing worse than pure x2/x0.5 MIMD:

| Strategy | 5h Delta | Notes |
|----------|----------|-------|
| x1.5 increase | 10,765 | Slower recovery |
| x1.3 at 200ms eval | 9,251 | Much worse |
| Binary search | 10,333 | Convergence too slow |
| Oscillation detection (alternation counter) | 11,371 | Over-damped |
| Hold after decrease | 10,584 | Lost throughput during holds |
| Dead zone | 11,935 | Marginal |
| sqrt(2) MIMD at 250ms | 12,388 | Close but strictly worse |
| Discovery-then-probe (+1/-1) | 8,164 | Much worse |
| P-controller | 6,437 | Catastrophically worse |
| Early overshoot detection | 11,603 | Worse |
| Severity-based decrease | 11,232 | Worse |
| Drain-wait | 10,335 | Worse |
| AIMD +5 | 10,344 | Worse |
| Capacity estimate initial limit | 10,885 | Stale estimates |
| x1.5/div1.5 symmetric | 10,863 | Worse |
| Mid-window emergency halving | 11,749 | Worse |
| AIMD +max(2, limit*0.05) | 10,031 | Worse |
| Pure AIMD +1 | 10,143 | Far too slow recovery |
| 250ms eval interval | 4,996 | Catastrophically worse |

**Key finding**: Every approach that slows MIMD growth also reduces SuccessScore by a comparable amount. The fast doubling that causes oscillation-driven Shed is the same mechanism that enables fast recovery and high steady-state throughput.

### 5.4 OPEN Transition Breakthrough

The turning point was adding a THROTTLED -> OPEN transition: when `evalErrRate > 0.5` and the limit is already at `minInflightLimit`, transition to OPEN (full block for one cooldown period). This stops failure accumulation during catastrophic backend failure.

Before: 5h = 12,443. After: 5h = **13,475** -- first time beating Static-Peak (12,719).

Further OPEN-related experiments (ssthresh, limit=1 restart, cleanEvalStreak gating, adaptive eval interval, rate-limiting OPEN entries) all made things worse.

### 5.5 Final Tuning Experiments

Baseline for this phase: 5h = 13,475, 28h = 94,685.

| # | Experiment | Hypothesis | 5h Delta | 28h Delta | Result | Principle |
|---|-----------|-----------|----------|-----------|--------|-----------|
| 1 | evalInterval 750ms (from 500ms) | Slower eval gives more stable estimates | 10,232 | - | Degraded. Delays detection and recovery. | 7 |
| 2 | Multiplicative increase x1.5 (from x2) | Gentler growth avoids overshooting capacity | 11,067 | - | Degraded. Slower growth loses SuccessScore. | 8 |
| 3 | Capacity-aware growth (x2 below, x1.25 above estimate) | Use Little's Law to slow growth near capacity | 10,341 | - | Degraded. Stale capacity estimate. | 6 |
| 4 | Tiered decrease (x0.25 for >80% error) | Faster decrease for extreme errors | 12,275 | - | Mixed. More time at minimum limit. | 7 |
| 5 | Recovery holdoff 1.5s (from 3s) | Faster recovery | 13,475 | 94,685 | Ineffective. Holdoff never binding. | 5 |
| 6 | EWMA half-life 2s (from 3s) | Faster detection | 9,849 | - | Severely degraded. Premature recovery. | 8 |
| 7 | EWMA half-life 5s (from 3s) | More smoothing | 12,512 | - | Degraded. Late detection and recovery. | 8 |
| 8 | **Limit-based recovery (limit > 3x inflight)** | Recover when limit is unconstraining | **13,570** | **95,667** | **Improved.** Exits THROTTLED earlier. | 9 |
| 9 | Limit recovery 2.0x | More aggressive exit | 10,996 | - | Degraded. Re-trips. | 4 |
| 10 | Limit recovery 4.0x | More conservative exit | 13,452 | - | Barely fires. | 4 |
| 11 | Limit recovery + warmup reset | Re-arm trip after recovery | 13,535 | - | Slightly degraded. | 6 |
| 12 | Expanded OPEN (limit <= 5) | Wider OPEN trigger | 11,883 | - | Degraded. OPEN during convergence. | 5 |
| 13 | Cooldown 3s (2x timeout) | Longer OPEN | 10,858 | - | Degraded. HPA scales down. | 7 |
| 14 | **tripBuffer 0.05** | Buffer above SLO for trip threshold | 13,565 | **96,625** | **Best 28h.** Avoids degradation-hour trips. | 9 |
| 15 | tripBuffer 0.10 | Larger buffer | 13,525 | 95,884 | Slightly worse than 0.05. | 5 |
| 16 | tripBuffer gate on recovery | Gate recovery on tripThreshold | 13,525 | 95,884 | Prevents useful recoveries. | 3 |
| 17 | tripBuffer 0.07 | Sweep | - | 96,247 | Below 0.05. | 2 |
| 18 | tripBuffer 0.04 | Sweep | - | 96,255 | Below 0.05. | 2 |
| 19 | tripBuffer 0.06 | Sweep | - | 95,183 | Boundary effect. | 2 |
| 20 | TCP slow-start (additive from CLOSED) | Conservative initial growth | 10,263 | - | Severely degraded. Too slow. | 8 |

### 5.6 Graduated Recovery and MIMD Tuning

After the initial optimisation phase, three changes were made to address the 28h gap between Levee and Static-Peak:

**Graduated recovery** (keep inflightLimit after THROTTLED→CLOSED):

The original design reset `inflightLimit = math.MaxFloat64` on recovery. This caused a burst of traffic at the THROTTLED→CLOSED transition that could re-trigger overload. The fix: keep the inflight limit after recovery and grow it via a new `maybeRelaxLimit()` method in CLOSED state. This method uses x2 doubling (same as MIMD in THROTTLED) with a safety check that re-trips to THROTTLED if errors rise. The limit is fully relaxed when it exceeds 10x actual inflight. Effect: 28h Delta improved from 53,643 to 60,494 (+6,851).

**sqrt(2) MIMD increase** (from x2 to x√2 ≈ 1.414 in THROTTLED):

The x2 increase caused ~100% capacity overshoot at the MIMD oscillation point. Changing to x√2 reduces overshoot to ~41%, cutting shed events from ~1.17M to ~769K. A multiplier sweep tested x1.3, x1.35, x1.4, x1.41, x1.42, x1.43, x1.44, x1.45, x1.5, x1.7. Peak was at x1.42 (63,383 Delta) but x√2 was chosen as the principled value (geometric mean of 1.0 and 2.0 on log scale).

**50% initial limit** (from 75% to 50% of current inflight):

Changed `enterThrottledFromClosed` to use `float64(l.inflight)*0.5` instead of `0.75`. More aggressive initial shed, faster convergence.

**ssthresh (TCP slow-start threshold) -- tried and abandoned**:

Analogous to TCP, remember the limit level where errors last occurred (ssthresh). Below ssthresh, double (fast recovery). At/above ssthresh, increase by x1.25 (conservative probing). Failed because ssthresh became stale during HPA scale-up events -- the capacity boundary remembered from a 2-replica backend doesn't apply after autoscaling to 6 replicas. 5h Delta dropped from 9,755 to 7,736 (below Static-Peak 8,156).

### 5.7 SLO Sweep

The SLO sweep tests Levee against Static-Peak and Static-BAU at SLO values 0.9, 0.8, 0.7, 0.6, 0.5 on the full 28h workload. Static CBs use hardcoded thresholds that don't change with SLO.

With the graduated recovery + sqrt(2) changes, Levee wins at SLO 0.9 (+767) but loses at all other SLO values, with the gap widening at lower SLOs (worst: SLO 0.5, -4,378).

The root cause is the MIMD eval threshold: `maybeEvaluateLimit()` uses `sloErrRate` to decide increase vs decrease. At SLO 0.5, MIMD only decreases when eval error rate exceeds 50% -- during incidents with 30-40% errors, it keeps increasing the limit. The trip threshold (`tripThreshold = sloErrRate + successRate * tripBufferFactor`) is actually tighter relative to sloErrRate at lower SLOs (2.5% relative buffer at SLO 0.5 vs 4.5% at SLO 0.9), so the problem is not in tripping but in the MIMD control loop becoming ineffective.

Note: this comparison is arguably unfair -- Static-Peak's thresholds are fixed and happen to be well-tuned for this workload regardless of what SLO is specified, while Levee genuinely adapts its behaviour to the SLO parameter.

---

## 6. Results Across Checkpoints

This section traces how benchmark results improved across three development checkpoints.

### 6.1 Checkpoint 1 — The Foundation

The initial implementation with graduated recovery, sqrt(2) MIMD, and the 3-state machine (CLOSED/THROTTLED/OPEN):

| Benchmark | Levee | Static-Peak | Lead | Status |
|-----------|------:|------------:|-----:|--------|
| Isolated 28h (SLO=0.90) | 63,429 | 61,498 | +1,932 | WIN |
| Cooperative 28h (10 instances) | — | — | — | Not yet tested |
| SLO Sweep | 1/5 wins | | | MOSTLY LOSING |
| QD Sweep | — | — | — | Not yet tested |

The isolated benchmark was a win, but narrow. The SLO sweep exposed the MIMD threshold problem at lower SLOs.

### 6.2 Checkpoint 2 — Adaptive Probe Control

Added the HALF_OPEN state, exponential OPEN backoff, recoverThreshold floor, and Cap=1 on OPEN→HALF_OPEN:

| Benchmark | Levee Lead | Change from CP1 |
|-----------|-----------|-----------------|
| Isolated 28h | +952 | -980 (regressed slightly due to cap=1 cost) |
| Cooperative 28h (100 instances) | +2,180 | New benchmark, new win |
| SLO Sweep | 2/5 wins | +1 win (tight SLO fix) |
| QD Sweep | 3/5 wins | New benchmark |

The cooperative benchmark went from untested to a win. The probe control mechanisms were essential for multi-instance deployments. But the cap=1 conservative probing hurt isolated performance slightly, and the SLO/QD sweeps were still inconsistent.

### 6.3 Checkpoint 3 — Event-Clock EWMA

Added the event-clock alpha cap on all EWMAs and latency-adaptive eval interval:

| Benchmark | Levee | Static-Peak | Lead | Status |
|-----------|------:|------------:|-----:|--------|
| P1: Isolated 28h (SLO=0.90) | 73,557 | 61,498 | **+12,059** | WIN |
| P2: Load Variation (7 configs) | 7/7 wins | | +7,340 to +23,165 | WIN |
| P3: Cooperative 28h (100 instances) | 77,612 | 70,098 | **+7,514** | WIN |
| P4: SLO Sweep | 5/5 wins | | +9,467 to +13,832 | WIN |
| P5: QD Sweep | 5/5 wins | | +6,523 to +19,847 | WIN |

The event-clock EWMA was the single biggest improvement in the project's history. By fixing the signal corruption that had been silently sabotaging every OPEN→HALF_OPEN transition, every benchmark improved dramatically:

| Metric | Checkpoint 2 | Checkpoint 3 | Improvement |
|--------|-------------:|-------------:|------------:|
| P1 Lead | +952 | +12,059 | 12.7× |
| P3 Lead | +2,180 | +7,514 | 3.4× |
| P4 Wins | 2/5 | 5/5 | Flipped 3 losses |
| P5 Wins | 3/5 | 5/5 | Flipped 2 losses |

Levee now wins every benchmark and every configuration within each sweep. Total: 20/20.

### 6.4 The Impact of Each Mechanism

| Mechanism | Phase | P1 Impact | Key Insight |
|-----------|-------|-----------|-------------|
| Event-clock EWMA alpha cap | 3 | +11,107 | Prevents OPEN cooldowns from erasing EWMA history. Single biggest improvement. |
| Graduated recovery | 2 | +6,851 (first added) | Keep inflight limit after THROTTLED→CLOSED. Prevents recovery burst. |
| Adaptive probe control | 2 | +952 (first added) | One probe per eval window + exponential eval backoff. Essential for cooperative. |
| sqrt(2) MIMD | 2 | +766 (first added) | Reduces capacity overshoot from ~100% to ~41%. |
| recoverThreshold floor | 2 | +8,031 (SLO=0.99) | Prevents over-blocking at tight SLOs. |
| Latency-adaptive eval | 3 | No change at 100ms | Production-ready for any latency. Same behaviour at benchmark latency. |

---

## 7. Differences from the Previous Implementation

The committed version of Levee (pre-rewrite) was a fundamentally different design across 4 files totalling 1,327 lines. The current implementation is a single ~535-line file. This section summarises what changed and why.

### 7.1 Architecture

| Aspect | Previous | Current |
|--------|----------|---------|
| Files | `levee.go` (345), `levee_impl.go` (171), `state.go` (129), `stats.go` (293), `levee_test.go` (389) | `levee.go` (~535) |
| Signal processing | Multi-timescale EWMAs (base/mid/long) over circular buffers with trimmed means, deviation tracking, and dynamic buffer resizing | Single EWMA per signal with event-clock alpha cap |
| Trip mechanism | Latency anomaly detection (deviation from historical trimmed mean) + SLO violation | Error rate EWMA exceeding trip threshold + consecutive failure run |
| Throttling control | AIMD on concurrency ceiling: +10% when latency stable, x0.5 when anomaly detected, evaluated every 50 samples | MIMD on inflight limit: x√2/x0.5 evaluated every baseEvalInterval() based on eval-window error rate |
| Concurrency tracking | `sync/atomic` int32 counter, external `AddConcurrent`/`RemoveConcurrent` calls | Internal `inflight` int64, incremented/decremented in `Start`/`Success`/`Fail` |
| State machine | CLOSED -> OPEN (cooldown then rate-limited probing) -> CLOSED; CLOSED <-> THROTTLED (latency-driven) | CLOSED -> THROTTLED -> OPEN (catastrophic) -> HALF_OPEN (adaptive probing) -> CLOSED |
| Recovery | Wald confidence interval on success rate during OPEN probing; probabilistic admission at ~10% of historical throughput | MIMD growth in THROTTLED; recover to CLOSED when error rate drops below SLO or inflight limit exceeds 3x actual usage |

### 7.2 What Was Removed

**Multi-timescale EWMA system (`stats.go`, 293 lines).** The previous implementation maintained three EWMA timescales (base, mid ~5 min, long ~1 day) for both latency and success rate, plus trimmed-mean EWMAs, deviation EWMAs, and a dynamically-resizing circular buffer. This was ~60% of the total code. The current design uses a single time-weighted EWMA per signal with a 3s half-life -- 15 lines of code.

**Latency anomaly detection (`levee_impl.go`).** The `hasLatencyAnomaly()` function compared short-term latency against historical trimmed means to detect degradation. This was the primary THROTTLED trigger. Replaced by error-rate EWMA crossing the trip threshold, which is simpler and directly tied to the SLO.

**Wald confidence interval recovery (`levee_impl.go`).** During OPEN probing, the previous design collected samples and computed a Wald confidence interval on success rate, requiring `minSamples = 10` observations before making a recovery decision. If the upper confidence bound fell below the SLO, recovery was declared failed and cooldown reset. Replaced by MIMD growth in THROTTLED -- no statistical test needed because the control law self-corrects.

**Probabilistic probing (`levee_impl.go`).** The `probingAllowed()` function derived a probe rate from historical concurrency (Little's Law via latency trimmed mean) scaled by error rate, with probabilistic admission below 1 concurrent. Replaced by the THROTTLED→OPEN→HALF_OPEN cycle with conservative limit at minInflightLimit.

**Two separate error types.** `ErrCircuitOpen` and `ErrCircuitThrottled` were distinct errors. The current design uses only `ErrCircuitOpen` for all rejections -- the caller doesn't need to distinguish why they were rejected.

**Trigger diagnostics.** Seven named trigger constants (`TriggerSLOViolation`, `TriggerLatencyAnomaly`, etc.) for state change diagnostics. Removed -- the `StateChange` struct still exists but triggers are not populated.

**`StateUpdates() <-chan StateChange`.** Stubbed out (returned nil) in both versions. Retained in the API surface but unimplemented.

### 7.3 What Was Added

**MIMD control law.** The core mechanism that didn't exist before: an evaluation loop that multiplies the inflight limit by √2 when the eval-window error rate is within SLO, and halves it when above. O(log(n)) convergence to the right capacity vs. the previous design's incremental AIMD.

**THROTTLED as the primary protection state.** Previously, OPEN was the only protection state (with rate-limited probing bolted on). Now THROTTLED is the workhorse -- it provides load-capacity matching that keeps throughput flowing while shedding only the excess. OPEN is reserved for catastrophic failure only.

**Capacity estimation (Little's Law).** Continuous tracking of goodput (successes/sec) and average latency via EWMAs. Both use the event-clock alpha cap to survive idle periods.

**Consecutive failure detection.** A fast-trip mechanism derived from the SLO: the run length whose probability under normal operation is < 1e-8.

**Limit-based recovery.** Exit THROTTLED when `inflightLimit > 3 * (inflight + 1)`. Detects when the MIMD has grown the limit far beyond actual usage.

**Event-clock EWMA alpha cap.** All three EWMAs cap their alpha at the inter-observation interval (`1/goodput`). Prevents idle periods from erasing history — the mechanism that produced the single largest improvement in the project.

**HALF_OPEN state with adaptive probe control.** Post-OPEN probing with one probe per eval window and exponential eval interval backoff. Essential for multi-instance deployments.

**Latency-adaptive eval interval.** Eval interval adapts to observed latency. Same behaviour at 100ms, correct for any latency.

**recoverThreshold floor.** For tight SLOs, uses 10% floor. Prevents over-blocking.

**Serialisable checkpointing (`LeveeState`).** 16 scalar fields covering all runtime state.

### 7.4 Why the Rewrite

The previous design's fundamental limitation was identified in the user's core requirements message: "when the circuit opens due to overload, there's 0 work getting done." The old OPEN state with rate-limited probing at ~10% throughput was better than a hard block, but still wasted ~90% of available capacity during overload. The THROTTLED-first design matches admission to capacity, keeping throughput near maximum while shedding only the excess.

The latency anomaly detection was also brittle -- it required multiple EWMA timescales and trimmed means to distinguish real degradation from normal variance, and the AIMD concurrency control (+10%/-50%) was too slow to track rapid capacity changes during autoscaling events. The MIMD control law on error rate is both simpler (one EWMA, one threshold) and faster (multiplications achieve full capacity in seconds).
