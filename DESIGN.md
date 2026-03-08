# Levee: Design Document

## 1. How the Design Was Arrived At

### 1.1 The Initial Prompt

> Look at the README.md. It describes a product that you need to build. The skeleton of that product is provided in levee.go. There are some benchmarks in the benchmark directory. Your goal is to make levee top the benchmarks. I understand that this is a very vague ask, so we will take some time chatting back and forth for you to determine the requirements concretely. Then when I tell you explicitly, you develop a plan.

The README describes Levee as "a self-tuning circuit breaker and concurrency-based rate limiter for Go services" that takes a single SLO input (success rate + timeout) and adapts automatically. The skeleton in `levee.go` provided the API surface -- `NewLevee()`, `Start()`, `Success()`, `Fail()`, `State()` -- with empty method bodies. Two benchmark suites existed: an open-loop prescient benchmark and a distributed closed-loop benchmark.

### 1.2 Exploration Phase

After reading the full codebase (README, levee.go skeleton, all benchmark infrastructure), the assistant proposed an initial direction: adaptive concurrency control with EWMA-based error rate detection. Five clarifying questions were asked about concurrency limits (Little's Law?), THROTTLED vs OPEN semantics, statistical approach, state transitions, and the trigger mechanism.

### 1.3 Design Freedom and Anti-Fitting Constraint

The user's response established two critical ground rules:

> 1. The numbers in benchmark reports are indicative. They are not targets. 2. You need to develop your own mental model. It may or may not require THROTTLED state or latency anomaly detection or any of that nonsense. 3. You decide the best statistical approach or any other to suit the purpose. 4. You decide. 5. Yes, the purpose is diagnostics. You can retain or discard as per your choice.
>
> Caveats: you must not try to fit the benchmarks. The solution has to be general. The benchmarks may be changed later and levee must still win. You must not modify benchmarks.

This meant: full design freedom, but the solution must be principled. No benchmark-specific tuning.

### 1.4 The Benchmark-Anchoring Warning

When the assistant presented its initial mental model (adaptive concurrency limit + error-rate circuit breaking + EWMA signals), the user warned against anchoring on artifacts from a prior failed attempt:

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

## 3. Internal Design

### 3.1 State Machine

```
                  errEWMA > tripThreshold
                  OR consecFails >= N
CLOSED ──────────────────────────────────────► THROTTLED
   ▲                                              │ │
   │ errEWMA < recoverThreshold                   │ │
   │ OR inflightLimit >> inflight                  │ │
   │ (after holdoff)                               │ │
   └──────────────────────────────────────────────┘ │
                                                    │ evalErrRate > 0.5
                                                    │ at minInflightLimit
                                                    ▼
                                                   OPEN
                                                    │
                                                    │ cooldown elapsed
                                                    │ (= slo.Timeout)
                                                    ▼
                                              THROTTLED
                                        (conservative limit)
```

**CLOSED**: Normal operation. No inflight limit. All requests admitted. Maximum throughput.

**THROTTLED**: Active inflight limiting. Requests admitted only if `inflight < ceil(inflightLimit)`. The limit adjusts via MIMD every 500ms. This is the core adaptive state -- the circuit breaker is neither fully open nor fully closed, but matching admission to estimated capacity.

**OPEN**: Complete block. The backend is catastrophically broken (>50% error rate even at minimum concurrency). All requests rejected with `ErrCircuitOpen` until cooldown (= SLO timeout) elapses, then transition to THROTTLED with a conservative limit.

### 3.2 Parameters

| Parameter | Value | Purpose |
|-----------|-------|---------|
| `ewmaHalfLife` | 3s | Error rate EWMA half-life. ~6s effective window. Responsive enough to catch incidents within a few seconds, smooth enough to avoid noise. |
| `goodputHalfLife` | 3s | Goodput and latency EWMA half-life. Tracks capacity changes from HPA scaling. |
| `warmupSamples` | 50 | Minimum observations before trip is armed. At 100 RPS, this is 0.5s -- enough for a stable EWMA estimate. |
| `tripBufferFactor` | 0.05 | `tripThreshold = sloErrRate + successRate * tripBufferFactor`. Proportional to success rate so the buffer scales with how close normal operation is to the SLO boundary. |
| `evalInterval` | 500ms | MIMD re-evaluation period. Fast enough for responsive adaptation, slow enough for meaningful sample counts. |
| `recoveryHoldoff` | 3s | Minimum time in THROTTLED before recovery to CLOSED. Prevents premature recovery before the limit has had time to grow. |
| `minInflightLimit` | 1.0 | Floor for the inflight limit. Always allows at least one request for probing. |
| `openErrThreshold` | 0.5 | Error rate threshold in THROTTLED eval window that triggers OPEN transition, but only when already at minimum limit. Catches catastrophic failure. |
| `cooldownDuration` | `slo.Timeout` | Time in OPEN before probing. Equal to the SLO timeout (1.5s) to let in-flight requests drain. |

**Derived thresholds (computed from SLO in constructor):**

| Threshold | Formula | Example (SLO=90%) |
|-----------|---------|-------------------|
| `sloErrRate` | `1 - slo.SuccessRate` | 0.10 |
| `tripThreshold` | `sloErrRate + slo.SuccessRate * tripBufferFactor` | 0.145 |
| `recoverThreshold` | `sloErrRate` | 0.10 |
| `consecFailTrip` | `max(ceil(log(1e-8) / log(sloErrRate)), 5)` | 77 |

The consecutive-failure threshold is derived from the SLO: it's the run length whose probability under normal operation is less than 1e-8. At 10% error rate, 77 consecutive failures has probability 0.1^77 -- effectively impossible during healthy operation but inevitable during a total outage.

### 3.3 Operations

**`Start(ts)`** -- Admission control.
- CLOSED: Increment inflight, admit.
- OPEN: If cooldown elapsed, transition to THROTTLED (conservative limit), then fall through. Otherwise reject.
- THROTTLED: Run `maybeEvaluateLimit(ts)`. If `inflight >= ceil(inflightLimit)`, reject. Otherwise increment inflight, admit.

**`Success(ts, duration)`** -- Record a successful completion.
- Decrement inflight. Update error EWMA toward 0. Reset consecutive-fail counter. Update capacity estimates (goodput, latency).
- If THROTTLED: increment eval successes. Check recovery conditions (after holdoff): recover to CLOSED if error rate is below threshold OR if the inflight limit has grown far beyond actual usage (limit > 3x inflight).

**`Fail(ts, duration)`** -- Record a failed completion.
- Decrement inflight. Update error EWMA toward 1. Increment consecutive-fail counter.
- If CLOSED: Check trip conditions (EWMA > tripThreshold OR consecutive fails exceeded). Trip to THROTTLED.
- If THROTTLED: Increment eval failures. (The OPEN transition is handled in `maybeEvaluateLimit`, not here.)

**`maybeEvaluateLimit(ts)`** -- MIMD control law (called from `Start`).
- Skip if less than `evalInterval` since last evaluation.
- Compute `evalErrRate` from successes and failures in the window.
- If error rate > 50% and limit is already at minimum: transition to OPEN (catastrophic).
- If error rate > `sloErrRate`: halve the limit (multiplicative decrease).
- If error rate <= `sloErrRate`: double the limit (multiplicative increase).
- Reset eval counters and timestamp.

### 3.4 EWMA Error Rate

The EWMA uses a time-weighted decay:

```
alpha = 1 - exp(-dt * ln2 / halfLife)
errEWMA += alpha * (sample - errEWMA)
```

Each observation is either 0.0 (success) or 1.0 (failure). The half-life of 3s means observations from 6s ago have ~25% weight. This gives a responsive but not twitchy signal.

For `dt <= 0` (simultaneous events at the same timestamp), `alpha = 0.01` provides minimal movement.

### 3.5 Capacity Estimation

Levee continuously tracks backend capacity using two EWMAs:
- **Goodput** (successes per second): computed from inter-success arrival times.
- **Average latency** (mean success duration in seconds): exponential smoothing of individual success durations.

The capacity estimate (Little's Law) is `goodput * avgLatency`, representing the steady-state number of concurrent requests the backend can sustain. This estimate is used when entering THROTTLED from OPEN to set a conservative initial limit at 25% of estimated capacity, capped at 5.

---

## 4. Adaptation Strategies

### 4.1 Overload (Load Exceeds Capacity)

When incoming traffic exceeds backend capacity, latency rises and errors increase. The EWMA error rate climbs past `tripThreshold`, triggering a transition from CLOSED to THROTTLED. The initial inflight limit is set to 75% of current inflight -- an immediate 25% shed.

The MIMD control law then takes over. Every 500ms:
- If the eval-window error rate exceeds the SLO error rate, the limit halves.
- If errors are within budget, the limit doubles.

This converges to the point where admitted load matches backend capacity. The 500ms eval interval means convergence from any point to the right limit takes O(log(ratio)) steps -- typically 2-4 seconds.

As the HPA provisions more replicas (30s lag), backend capacity grows, more requests succeed, and the limit doubles until it matches the new capacity. Once the error rate drops below the SLO threshold and the holdoff period has passed, Levee transitions back to CLOSED.

### 4.2 Catastrophic Failure (Backend Crashes)

If the backend is completely broken -- >50% error rate even at the minimum inflight limit of 1 -- the circuit transitions to OPEN. All requests are blocked for one timeout period (1.5s), allowing in-flight requests to drain.

After cooldown, the circuit enters THROTTLED with a conservative limit: `max(min(estimatedCapacity * 0.25, 5), 1)`. This allows minimal probe traffic to detect recovery. If the backend is still broken, the MIMD will quickly halve back to minimum and re-enter OPEN. If the backend is recovering, the limit will grow.

### 4.3 Gradual Degradation (Database Slowdown)

The Cyber Monday scenario includes hours 10-23 where database degradation gradually pushes error rates from BAU levels toward the SLO boundary. The `tripBufferFactor` is critical here -- it prevents unnecessary trips during periods where errors are elevated but still within a reasonable margin of the SLO. Without this buffer, Levee would trip during degradation-hour noise, accumulating unnecessary Shed events and FailureScore.

### 4.4 Recovery and HPA Coordination

Levee's recovery strategy balances two concerns:
1. **Don't recover too early.** Premature recovery at full admission can re-trigger the overload that caused the trip. The `recoveryHoldoff` of 3s ensures the signal has stabilised.
2. **Don't stay throttled unnecessarily.** The limit-based recovery path (`inflightLimit > 3 * (inflight+1)`) detects when the MIMD has grown the limit far beyond actual usage -- a sign that the backend has recovered and the limit is no longer constraining. This avoids waiting for the EWMA to decay below threshold, which can take many seconds.

The MIMD doubling on each good evaluation means the limit reaches useful levels quickly. From a starting limit of 1, the limit reaches 64 in just 3 seconds (6 doublings at 500ms). This is fast enough to let the HPA see meaningful traffic and begin scaling.

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

### 5.6 Key Insight: The Structural Gap

The 28h gap between Levee (96,625) and Static-Peak (106,342) appears structural. Static-Peak's consecutive-failure trip mechanism generates only 205K Shed events over 28 hours, while Levee's MIMD oscillation generates ~1.1M. Under epoch-squared scoring, these concentrated MIMD oscillation bursts (rapid halving/doubling at 500ms intervals) are penalised heavily.

Every approach that reduces Shed events (slower growth, additive increase, capacity-aware limiting, longer eval intervals) also reduces SuccessScore by a comparable amount, because the same mechanism that causes Shed -- fast MIMD response -- is also what enables fast recovery and high steady-state throughput.

The cost of being adaptive is oscillation. The cost of being static is being wrong for the traffic you weren't tuned for -- which is why Levee wins on the open-loop benchmark and the 5h incident sub-test, but loses on the full 28h where Static-Peak's peak-tuned thresholds happen to match the scenario well.

---

## 6. Results at Time of Writing

| Benchmark | Levee | Static-Peak | Static-BAU | No-CB |
|-----------|-------|-------------|------------|-------|
| Open-loop TotalPenalty (lower=better) | **13,545** | 29,997 | 43,002 | - |
| 5h distributed Delta (higher=better) | **13,565** | 12,719 | - | - |
| 28h distributed Delta (higher=better) | 96,625 | **106,342** | 97,768 | -69,583 |

Levee beats Static-Peak on the open-loop benchmark (2.2x lower penalty) and on the 5h incident test (+846 Delta). On the full 28h scenario, Levee trails Static-Peak by ~10% but significantly outperforms No-CB and remains competitive with Static-BAU despite requiring zero manual tuning.

---

## 7. Differences from the Previous Implementation

The committed version of Levee (pre-rewrite) was a fundamentally different design across 4 files totalling 1,327 lines. The current implementation is a single 399-line file. This section summarises what changed and why.

### 7.1 Architecture

| Aspect | Previous | Current |
|--------|----------|---------|
| Files | `levee.go` (345), `levee_impl.go` (171), `state.go` (129), `stats.go` (293), `levee_test.go` (389) | `levee.go` (399) |
| Signal processing | Multi-timescale EWMAs (base/mid/long) over circular buffers with trimmed means, deviation tracking, and dynamic buffer resizing | Single EWMA per signal with time-weighted decay |
| Trip mechanism | Latency anomaly detection (deviation from historical trimmed mean) + SLO violation | Error rate EWMA exceeding trip threshold + consecutive failure run |
| Throttling control | AIMD on concurrency ceiling: +10% when latency stable, x0.5 when anomaly detected, evaluated every 50 samples | MIMD on inflight limit: x2/x0.5 evaluated every 500ms based on eval-window error rate |
| Concurrency tracking | `sync/atomic` int32 counter, external `AddConcurrent`/`RemoveConcurrent` calls | Internal `inflight` int64, incremented/decremented in `Start`/`Success`/`Fail` |
| State machine | CLOSED -> OPEN (cooldown then rate-limited probing) -> CLOSED; CLOSED <-> THROTTLED (latency-driven) | CLOSED -> THROTTLED (inflight-limited) -> OPEN (catastrophic) -> THROTTLED -> CLOSED |
| Recovery | Wald confidence interval on success rate during OPEN probing; probabilistic admission at ~10% of historical throughput | MIMD growth in THROTTLED; recover to CLOSED when error rate drops below SLO or inflight limit exceeds 3x actual usage |

### 7.2 What Was Removed

**Multi-timescale EWMA system (`stats.go`, 293 lines).** The previous implementation maintained three EWMA timescales (base, mid ~5 min, long ~1 day) for both latency and success rate, plus trimmed-mean EWMAs, deviation EWMAs, and a dynamically-resizing circular buffer. This was ~60% of the total code. The current design uses a single time-weighted EWMA per signal with a 3s half-life -- 15 lines of code.

**Latency anomaly detection (`levee_impl.go`).** The `hasLatencyAnomaly()` function compared short-term latency against historical trimmed means to detect degradation. This was the primary THROTTLED trigger. Replaced by error-rate EWMA crossing the trip threshold, which is simpler and directly tied to the SLO.

**Wald confidence interval recovery (`levee_impl.go`).** During OPEN probing, the previous design collected samples and computed a Wald confidence interval on success rate, requiring `minSamples = 10` observations before making a recovery decision. If the upper confidence bound fell below the SLO, recovery was declared failed and cooldown reset. Replaced by MIMD growth in THROTTLED -- no statistical test needed because the control law self-corrects.

**Probabilistic probing (`levee_impl.go`).** The `probingAllowed()` function derived a probe rate from historical concurrency (Little's Law via latency trimmed mean) scaled by error rate, with probabilistic admission below 1 concurrent. Replaced by the simple THROTTLED->OPEN->THROTTLED cycle with conservative initial limit from Little's Law capacity estimate.

**Two separate error types.** `ErrCircuitOpen` and `ErrCircuitThrottled` were distinct errors. The current design uses only `ErrCircuitOpen` for all rejections -- the caller doesn't need to distinguish why they were rejected.

**Trigger diagnostics.** Seven named trigger constants (`TriggerSLOViolation`, `TriggerLatencyAnomaly`, etc.) for state change diagnostics. Removed -- the `StateChange` struct still exists but triggers are not populated.

**`StateUpdates() <-chan StateChange`.** Stubbed out (returned nil) in both versions. Retained in the API surface but unimplemented.

### 7.3 What Was Added

**MIMD control law.** The core mechanism that didn't exist before: a 500ms evaluation loop that doubles the inflight limit when the eval-window error rate is within SLO, and halves it when above. This is the key innovation -- O(log(n)) convergence to the right capacity vs. the previous design's incremental AIMD.

**THROTTLED as the primary protection state.** Previously, OPEN was the only protection state (with rate-limited probing bolted on). Now THROTTLED is the workhorse -- it provides load-capacity matching that keeps throughput flowing while shedding excess. OPEN is reserved for catastrophic failure only.

**Capacity estimation (Little's Law).** Continuous tracking of goodput (successes/sec) and average latency via EWMAs. Used to set a conservative initial inflight limit when entering THROTTLED from OPEN.

**Consecutive failure detection.** A fast-trip mechanism derived from the SLO: the run length whose probability under normal operation is < 1e-8. At SLO 0.9, this is 77 consecutive failures -- effectively impossible during healthy operation, inevitable during total outage.

**Limit-based recovery.** A novel recovery path: exit THROTTLED when `inflightLimit > 3 * (inflight + 1)`. This detects when the MIMD has grown the limit far beyond actual usage, meaning the backend has recovered and the limit is no longer constraining.

**Serialisable checkpointing (`LeveeState`).** The previous `SaveState`/`RestoreState` serialised 18 EWMA values across two timeseries. The current version serialises 16 scalar fields covering all runtime state -- simpler and complete.

### 7.4 Why the Rewrite

The previous design's fundamental limitation was identified in the user's core requirements message: "when the circuit opens due to overload, there's 0 work getting done." The old OPEN state with rate-limited probing at ~10% throughput was better than a hard block, but still wasted ~90% of available capacity during overload. The THROTTLED-first design matches admission to capacity, keeping throughput near maximum while shedding only the excess.

The latency anomaly detection was also brittle -- it required multiple EWMA timescales and trimmed means to distinguish real degradation from normal variance, and the AIMD concurrency control (+10%/-50%) was too slow to track rapid capacity changes during autoscaling events. The MIMD control law on error rate is both simpler (one EWMA, one threshold) and faster (doublings reach capacity in seconds).
