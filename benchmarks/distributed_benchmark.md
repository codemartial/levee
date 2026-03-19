# Distributed Benchmark

## Goal

To evaluate Levee against static circuit breakers in a realistic closed-loop simulation where circuit breaker decisions affect backend load, capacity scales dynamically, request queuing creates backpressure, and sustained overload crashes the backend.

## Why a Distributed Benchmark?

The [open-loop benchmark](benchmark.md) evaluates circuit breakers against a prescient oracle, but has a key limitation: CB decisions don't affect the simulated backend. In reality:

- Blocking requests reduces backend load, helping it recover faster
- Allowing requests during overload worsens the situation
- Autoscalers respond to actual load, not theoretical load
- Queue backpressure creates realistic latency patterns
- Sustained overload crashes backend nodes, causing extended outages

This distributed benchmark creates a closed-loop simulation where these dynamics play out.

## Architecture

Each circuit breaker runs in-process with its own dedicated backend instance:

```
[Load Generator] ──> [App Server] ──> [Backend Server]
                     (1 CB)           (dedicated instance)
```

All four CB benchmarks run in parallel, each with:
- Identical load patterns (same seed for deterministic request generation)
- Identical backend configuration (same capacity, queue depth, autoscaling)
- Independent backend state (separate CapacityController, WorkerPool, RNG)

Logical time simulation ensures deterministic, reproducible results.

## Components

### Load Generator
- Generates requests based on load specs (RPS, error rate, duration)
- Uses **logical time** - requests carry timestamps, no wall-clock sleeping
- Deterministic via seed - same seed produces identical request streams

### App Server (Circuit Breaker)
- Hosts the circuit breaker under test
- For each request: CB decides allow/block, then calls backend if allowed
- Records success/failure and updates CB state

### Backend Server
- Simulates a capacity-limited service with autoscaling and crash recovery
- **Worker Pool**: Concurrency-limited request processing with load-dependent degradation (latency inflation, error escalation)
- **Capacity Controller**: HPA-style autoscaler (1-8 replicas, 150 RPS each, 30s provisioning lag)
- **Request Queue**: Per-replica queue depth absorbs burst arrivals from exponential inter-arrival times
- **Crash Simulation**: Sustained extreme load (EWMA > 3.0 for 30s) triggers a crash with 5-minute downtime. During crash, all requests fail and HPA is frozen. On recovery, replicas reset to minimum and the autoscaler must scale back up through normal provisioning — total recovery takes ~5.5 minutes.
- **Latency Generator**: Samples realistic latencies from load spec distributions
- All timing uses **logical time** from request timestamps

## The Workload

Same Cyber Monday scenario as the open-loop benchmark:

- **Hours 0-4**: Calm baseline (100 RPM, 1% errors)
- **Hour 4**: Midnight spike - 15x traffic, backend falls over (60% errors)
- **Hours 5-9**: Hourly sale events with 3-5x spikes, brief degradation (6-10% errors)
- **Hour 10**: Morning peak - 48x baseline, 25-35% errors
- **Hours 10-18**: BAU creeps up, latency/errors increase (degradation 1x→1.5x)
- **Hour 18**: Evening peak - another 48x spike
- **Hours 18-24**: Continued degradation (1.5x→3x) as DB slows

Total: 28 hours of simulated traffic, ~56M requests per CB.

## Circuit Breakers Under Test

- **Levee**: Adaptive circuit breaker and concurrency limiter. Takes a single SLO input (success rate + timeout) and self-tunes using MIMD inflight limiting, EWMA error tracking, and exponential probe backoff. The subject under test.
- **Static-Peak**: Tuned for peak traffic (2,000-5,000 RPS). Trips after 7 consecutive failures, recovers after 5 consecutive successes, 30s half-open timeout, 3 concurrent probe calls. The primary competitor — its conservative, well-tuned thresholds make it a strong baseline.
- **Static-BAU**: Tuned for BAU traffic (100-800 RPS). Trips after 5 consecutive failures, recovers after 3 consecutive successes, 10s half-open timeout, 1 probe call. More aggressive tripping and faster recovery, but poorly suited to peak traffic.
- **No-CB**: No circuit breaker. All requests pass through. Included as a negative control to demonstrate the cost of no protection.

## Scoring

### Throughput-Weighted Epoch Scoring

Raw success/failure counts don't capture business impact. We use throughput-weighted scoring:

**Per 200ms epoch:**
- Count successes (`num_s`) and failures (`num_f`) in the window
- `success_score_epoch` = num_s² × 5
- `failure_score_epoch` = num_f² × 5

**Final scores:**
- `SuccessScore` = √(sum of all `success_score_epoch`)
- `FailureScore` = √(sum of all `failure_score_epoch`)
- `Delta` = (SuccessScore - FailureScore) × Allowed/(Allowed+Blocked) (higher is better)

### Why This Scoring?

The squared weighting rewards **desirable high-load behaviour**:
- 100 successes in one epoch scores higher than 10 successes across 10 epochs
- High-throughput periods (peak traffic) contribute more to the score
- Allowing high failure rates is penalised more heavily

This captures the business reality: maintaining throughput during peak traffic is more valuable than during quiet periods.

## Running the Benchmark

**First incident (5 hours):**
```bash
go test -v -run TestDistributedBenchmarkFirstIncident -timeout 15m
```

**Full test (28 hours, ~2 minutes):**
```bash
go test -v -run '^TestDistributedBenchmark$' -timeout 60m
```

**SLO sweep (28 hours × 5 SLO values, ~10 minutes):**
```bash
go test -v -run TestSLOSweep -timeout 60m
```

**Queue depth sweep (28 hours × 5 queue depths, ~10 minutes):**
```bash
go test -v -run TestQueueDepthSweep -timeout 60m
```

**Cooperative benchmark (100 instances sharing 1 backend, ~3 minutes):**
```bash
go test -v -run TestCooperationBenchmark -timeout 60m
```

**Load variation sweep (7 workload variations, ~16 minutes):**
```bash
go test -v -run TestLoadVariationSweep -timeout 120m
```

## Results

### P1: Isolated Benchmark (28 hours simulated)

**Objective**: Verify Levee outperforms static CBs on the reference Cyber Monday workload with a dedicated backend per instance.

Each CB runs with its own dedicated backend instance. SLO=0.90, QD=50.

```
Candidate       |    Blocked |    Allowed |  Successes |  Failures | SuccessScore | FailureScore |      Delta | MaxConcurrency
----------------+------------+------------+------------+-----------+--------------+--------------+-----------+----------------
No-CB           |          0 |   56518826 |   31155708 |  25363118 |    132026.73 |    201637.85 |  -69611.12 |           7494
Levee           |   21832578 |   34686248 |   33880307 |    805941 |    135522.61 |     15666.97 |   73556.78 |           5115
Static-BAU      |   25892298 |   30626528 |   29822483 |    804045 |    127370.11 |     29589.31 |   52985.64 |           7044
Static-Peak     |   26052608 |   30466218 |   30073560 |    392658 |    130072.05 |     15985.79 |   61497.68 |           5086

Delta = (SuccessScore - FailureScore) * Allowed/(Allowed+Blocked)  [higher is better]

Backend Processing Stats:
Candidate       |   Requests |  Successes |   Failures |       Shed | QueueDrops |    Crashes
----------------+------------+------------+------------+------------+------------+------------
No-CB           |   56518826 |   31159395 |   25359431 |    2754143 |     329407 |         78
Levee           |   34686248 |   33880307 |     805941 |     108660 |     218995 |          0
Static-BAU      |   30626528 |   29822483 |     804045 |     504984 |      70694 |         15
Static-Peak     |   30466218 |   30073560 |     392658 |     184129 |      17530 |         50
```

### P2: Load Variation Sweep (28 hours × 7 workload variations)

**Objective**: Verify Levee hasn't overfitted to the Cyber Monday workload parameters by varying RPM and error rate independently.

Tests 7 combinations of RPM scaling (0.5×, 1.0×, 2.0×) and error rate scaling (0.2×, 1.0×, 3.0×). Each variation runs the full 28h workload with scaled parameters. Backend configuration is fixed (same capacity, same 8-replica HPA cap) — the variations change how much load exceeds capacity, not the capacity itself.

```
Variation                 |        Levee |  Static-Peak |   Levee Lead
--------------------------+--------------+--------------+-------------
baseline (1.0×, 1.0×)    |     73556.78 |     61497.68 |    +12059.09 WIN
half-RPM (0.5×, 1.0×)    |     39790.10 |     32181.96 |     +7608.14 WIN
double-RPM (2.0×, 1.0×)  |     67671.39 |     30173.17 |    +37498.22 WIN
low-err (1.0×, 0.2×)     |     72596.00 |     62947.18 |     +9648.83 WIN
high-err (1.0×, 3.0×)    |     78099.13 |     60636.40 |    +17462.74 WIN
low-RPM-high-err (0.5×, 3.0×) | 45448.72 |    34962.62 |    +10486.10 WIN
high-RPM-low-err (2.0×, 0.2×) | 66558.94 |    30737.44 |    +35821.50 WIN
```

Levee wins all 7 variations. Strongest advantage under extreme overload (double-RPM: +37,498) where the 8-replica cap means the backend physically cannot scale to meet demand — Levee's concurrency control matches admission to capacity while Static-Peak's binary open/close cannot. Smallest advantage at half-RPM (+7,608) where less overload means less opportunity for adaptive benefit.

### P3: Cooperative Benchmark (28 hours, 100 instances)

**Objective**: Verify Levee performs well when many instances share a single backend, where aggregate probe pressure can overwhelm a degraded service.

100 CB instances share a single backend. Requests are dispatched round-robin. SLO=0.80, QD=50.

```
CB Type         | Instances |    Allowed |  Successes |  Failures | SuccessScore | FailureScore |      Delta | MaxConcurrency | Crashes
No-CB           |       100 |   56518826 |   31155708 |  25363118 |    132026.73 |    201637.85 |  -69611.12 |             75 |      78
Levee           |       100 |   36704170 |   35289653 |   1414517 |    138179.20 |     18668.63 |   77611.95 |             60 |       0
Static-BAU      |       100 |   38214007 |   35580171 |   2633836 |    140005.72 |     39868.43 |   67705.71 |             74 |       0
Static-Peak     |       100 |   36416323 |   34794872 |   1621451 |    137761.86 |     28967.67 |   70098.49 |             75 |       0

Backend Processing Stats:
CB Type         |   Requests |  Successes |   Failures |       Shed | QueueDrops |    Crashes
Levee           |   36704170 |   35289653 |    1414517 |     109537 |     386624 |          0
Static-Peak     |   36416323 |   34794872 |    1621451 |     609839 |     489818 |          0
```

### P4: SLO Sweep (28 hours × 5 SLO values)

**Objective**: Verify Levee adapts correctly across different SLO configurations, from tight (0.99) to relaxed (0.70).

Static CBs use hardcoded thresholds and are unaffected by the SLO parameter.

```
SLO    |        Levee |  Static-Peak |   Levee Lead
-------+--------------+--------------+-------------
0.99   |     75178.82 |     61497.68 |    +13681.13  <-- Levee wins
0.95   |     70964.80 |     61497.68 |     +9467.11  <-- Levee wins
0.90   |     73556.78 |     61497.68 |    +12059.09  <-- Levee wins
0.80   |     73009.70 |     61497.68 |    +11512.02  <-- Levee wins
0.70   |     75329.33 |     61497.68 |    +13831.65  <-- Levee wins
```

Levee wins all 5 SLO configurations with leads ranging from +9,467 to +13,832. The `recoverThreshold` floor of 0.10 prevents over-blocking at tight SLOs, while event-clock EWMA ensures stable signals across all configurations.

### P5: Queue Depth Sweep (28 hours × 5 queue depths)

**Objective**: Verify Levee adapts to different backend queue configurations, from shallow (fast-shedding) to deep (crash-prone).

Base queue depth is 50 per replica.

```
QueueDepth   |        Levee |  Static-Peak |   Levee Lead |  Crashes (L/SP)
-------------+--------------+--------------+--------------+-----------------
  10 (0.2x)  |     83587.44 |     63740.62 |    +19846.82 |      0 /   0
  25 (0.5x)  |     78371.18 |     65115.33 |    +13255.85 |      0 /   1
  50 (1.0x)  |     73556.78 |     61497.68 |    +12059.09 |      0 /  50
  70 (1.4x)  |     67946.03 |     61422.87 |     +6523.17 |      4 /  65
 100 (2.0x)  |     69704.39 |     53028.48 |    +16675.90 |      8 /  85
```

Levee wins all 5 queue depth configurations with leads ranging from +6,523 to +19,847.

- **Strongest at extremes**: At shallow queues (0.2x), Levee's concurrency control prevents overload. At deep queues (2.0x), Levee's throttling prevents the cascading crashes that afflict Static-Peak (85 crashes).
- **Crash resilience**: Levee has 0-8 crashes across all configs. Static-Peak has 0-85.

## Analysis

1. **Levee wins every configuration tested**: 1/1 isolated, 7/7 load variations, 1/1 cooperative, 5/5 SLO sweep, 5/5 QD sweep — **20/20 total**. No evidence of overfitting to any specific workload parameter.

2. **Largest advantage under extreme overload**: The double-RPM variations (+37,498 and +35,822) show Levee's strongest relative performance. When load far exceeds the backend's maximum capacity (8 replicas × 150 RPS), Levee's MIMD concurrency control matches admission to actual capacity. Static-Peak's binary open/close admits 29.5M requests vs Levee's 49.3M — Levee delivers 67% more throughput with only 3× the failure count, for more than double the Delta.

3. **Higher throughput with controlled failure rate**: Across all configurations, Levee consistently admits more requests than Static-Peak while maintaining a higher or comparable SuccessScore. The higher failure count is more than offset by the throughput gain under epoch-squared scoring.

4. **Zero crashes in isolated and cooperative benchmarks**: Levee's concurrency control prevents backend crashes entirely in the primary benchmarks. Only the most extreme queue depth configurations (1.4x, 2.0x) produce any crashes (4-8 vs Static-Peak's 65-85).

5. **Cooperative advantage**: With 100 instances sharing a backend, Levee's adaptive probe control (exponential eval interval backoff + one-probe-per-window rate limiting) reduces aggregate probe pressure from ~1,000/s to ~12.5/s during sustained degradation. This lets the backend recover faster than under Static-Peak's fixed cooldowns.

6. **Robust across SLO configurations**: The `recoverThreshold` floor of 0.10 and event-clock EWMA ensure Levee wins at every SLO from 0.70 to 0.99, with leads of +9,467 to +13,832. Static-Peak's hardcoded thresholds cannot adapt to different SLO targets.

7. **Static-BAU is consistently the worst CB**: Across all benchmarks, Static-BAU trails both Levee and Static-Peak. Its BAU-tuned thresholds (5 consecutive failures to trip, 10s half-open timeout) are too aggressive for peak traffic — it trips frequently during spikes, but its short cooldown means it recovers and re-trips in rapid cycles. This flapping produces high FailureScore (29,589 isolated vs Static-Peak's 15,986) without the throughput benefit of staying open. At double-RPM it scores 28,928 Delta vs Static-Peak's 30,173 and Levee's 67,671. Its MaxConcurrency is also consistently the highest (7,044 isolated, 14,220 at double-RPM), indicating poor concurrency control — it allows traffic bursts between flap cycles that push the backend toward crashes.

8. **No-CB crashes 78 times**: Without protection, sustained overload crashes the backend repeatedly, resulting in 44.9% failure rate and deeply negative Delta (-69.6K).

## Technical Notes

### Logical Time

The entire simulation operates on **logical time** from request timestamps:
- No wall-clock sleeping - requests processed as fast as possible
- 28 hours simulates in ~2 minutes wall time
- Autoscaler, queue, latency generation all use logical time
- Deterministic and reproducible results

### In-Process Isolation

Each CB runs with its own backend instance:
- Separate `backend.Server` with independent capacity/queue/crash state
- Separate `CapacityController` instance
- Same random seed ensures identical request patterns
- No shared state between CBs

### Backend Configuration

- Base capacity: 150 RPS per replica
- Queue depth: 50 per replica
- Min/Max replicas: 1-8
- Target utilization: 70%
- Evaluation interval: 15s
- Scale-down stabilization: 300s
- Scale-down delay: 600s (after scale-up)
- Provisioning lag: 30s
- Crash threshold: EWMA load > 3.0 sustained for 30s
- Crash downtime: 5 minutes (replicas reset to minimum on recovery)

### Crash Mechanics

When backend EWMA load exceeds 3.0 for 30 consecutive seconds:
1. The worker pool crashes — all in-flight requests fail
2. All requests return "service unavailable" for 5 minutes
3. HPA is frozen during crash (no scaling decisions, no demand recording)
4. On recovery, replicas reset to `MinReplicas` (pods are gone)
5. HPA retains its RPS estimate and immediately begins scaling up
6. Normal 30s provisioning lag applies — full capacity returns ~5.5 minutes after crash

This models real-world Kubernetes pod crashes where sustained resource exhaustion kills nodes and HPA must cold-start new pods.

## Comparison with Open-Loop Benchmark

| Aspect | Open-Loop | Distributed (Closed-Loop) |
|--------|-----------|---------------------------|
| Backend state | Static (from load spec) | Dynamic (responds to load) |
| Autoscaling | None | HPA-style (1-8 replicas) |
| Queuing | None | Realistic backpressure |
| Crash simulation | None | 5-min downtime on sustained overload |
| CB interference | All CBs see same backend | Each CB has own backend instance |
| Scoring | Prescient-relative | Throughput-weighted |
| Runtime | ~5 seconds | ~2 minutes |

Both benchmarks show Levee outperforming static configurations. The distributed benchmark additionally reveals that Levee's concurrency control prevents backend crashes and maintains stable performance across varying backend configurations (queue depth, SLO).
