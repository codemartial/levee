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

**Short test (4 hours, ~3 minutes):**
```bash
go test -v ./benchmarks -run TestDistributedBenchmarkShort -timeout 10m
```

**First incident (5 hours, ~5 minutes):**
```bash
go test -v ./benchmarks -run TestDistributedBenchmarkFirstIncident -timeout 15m
```

**Full test (28 hours, ~2.5 minutes):**
```bash
go test -v ./benchmarks -run TestDistributedBenchmark -timeout 60m
```

## Results

### Full Benchmark (28 hours simulated)

```
Candidate       |    Blocked |    Allowed |  Successes |  Failures | SuccessScore | FailureScore |      Delta | MaxConcurrency
----------------+------------+------------+------------+-----------+--------------+--------------+-----------+----------------
No-CB           |          0 |   56518826 |   31159395 |  25359431 |    132029.28 |    201611.95 |  -69582.67 |           7494
Levee           |   49254518 |    7264308 |    6453678 |    810630 |     39790.70 |     24156.81 |    2009.41 |           6212
Static-BAU      |   25890189 |   30628637 |   29822316 |    806321 |    127369.83 |     29602.23 |   52982.14 |           7044
Static-Peak     |   28106990 |   28411836 |   28017326 |    394510 |    123716.17 |     17373.86 |   53457.95 |           6801

Delta = (SuccessScore - FailureScore) * Allowed/(Allowed+Blocked)  [higher is better]

Backend Processing Stats:
Candidate       |   Requests |  Successes |   Failures |       Shed | QueueDrops |    Crashes
----------------+------------+------------+------------+------------+------------+------------
No-CB           |   56518826 |   31159395 |   25359431 |    2754143 |     329407 |         77
Levee           |    7264308 |    6453678 |     810630 |     603002 |      66702 |        174
Static-BAU      |   30628637 |   29822316 |     806321 |     506067 |      71705 |         14
Static-Peak     |   28411836 |   28017326 |     394510 |     205047 |      12236 |         56
```

## Analysis

> [!NOTE]
> The following analysis is out-dated due to poor performance of Levee on the revised close-loop benchmark.

1. **No-CB crashes 77 times**: Without protection, sustained overload crashes the backend repeatedly. Each crash causes 5 minutes of total downtime plus ~30s HPA recovery, resulting in 44.9% failure rate and deeply negative Delta (-69.6K).

2. **Levee achieves highest Delta** (+112,487) — 15% better than Static-Peak, 15% better than Static-BAU. Levee achieves this with the fewest backend crashes (11) and lowest failure rate (1.2%).

3. **Concurrency control is the differentiator**: Levee's MaxConcurrency of 1,913 is 3.5-3.9x lower than every other candidate. This keeps backend load manageable, preventing the sustained overload that triggers crashes. Static-Peak and Static-BAU allow 6,800-7,000 concurrent requests despite blocking similar total traffic — they block via binary open/close decisions rather than fine-grained throttling.

4. **Static-BAU crashes least (14) but scores worst among CBs**: Its aggressive blocking pattern (35 flaps in the open-loop benchmark) happens to prevent sustained overload, but the flapping causes high FailureScore (29.6K) from inconsistent protection.

5. **Static-Peak crashes most among CBs (56)**: Its conservative tuning allows sustained high concurrency through to the backend, triggering crash cascades similar to No-CB.

Levee provides the **best business outcome** with **zero configuration**: highest Delta, lowest failure rate, fewest crashes, and dramatically lower backend concurrency. The concurrency control that Levee applies is invisible to the open-loop benchmark but proves decisive in a realistic closed-loop simulation where backend health depends on the circuit breaker's behaviour.

## Technical Notes

### Logical Time

The entire simulation operates on **logical time** from request timestamps:
- No wall-clock sleeping - requests processed as fast as possible
- 28 hours simulates in ~2.5 minutes wall time
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
| Runtime | ~5 seconds | ~2.5 minutes |

Both benchmarks show Levee outperforming static configurations, but the distributed benchmark reveals an additional advantage: Levee's concurrency control prevents backend crashes that the open-loop benchmark cannot capture.
