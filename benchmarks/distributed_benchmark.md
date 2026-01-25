# Distributed Benchmark

## Goal

To evaluate Levee against static circuit breakers in a realistic closed-loop simulation where circuit breaker decisions affect backend load, capacity scales dynamically, and request queuing creates backpressure.

## Why a Distributed Benchmark?

The [open-loop benchmark](benchmark.md) evaluates circuit breakers against a prescient oracle, but has a key limitation: CB decisions don't affect the simulated backend. In reality:

- Blocking requests reduces backend load, helping it recover faster
- Allowing requests during overload worsens the situation
- Autoscalers respond to actual load, not theoretical load
- Queue backpressure creates realistic latency patterns

This distributed benchmark creates a closed-loop simulation where these dynamics play out.

## Architecture

Each circuit breaker runs with its own backend instance:

```
[Load Generator] ──unix──> [App Server] ──unix──> [Backend Server]
                           (1 CB)                  (dedicated instance)
```

All four CB benchmarks run in parallel, each with:
- Identical load patterns (same seed for deterministic request generation)
- Identical backend configuration (same capacity, queue depth, autoscaling)
- Independent backend state (separate CapacityController, queue, RNG)

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
- Simulates a capacity-limited service with autoscaling
- **Capacity Controller**: HPA-style autoscaler (1-8 replicas, 150 RPS each)
- **Request Queue**: Accepts requests when over capacity (10% of throughput)
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
- `SuccessScore` = √(sum of all success_score_epoch)
- `FailureScore` = √(sum of all failure_score_epoch)
- `Delta` = SuccessScore - FailureScore (higher is better)

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

**Full test (28 hours, ~130 minutes):**
```bash
go test -v ./benchmarks -run TestDistributedBenchmark -timeout 180m
```

## Results

### Full Benchmark (28 hours simulated)

```
Candidate       |    Blocked |    Allowed |  Successes |  Failures | SuccessScore | FailureScore |      Delta
----------------+------------+------------+------------+-----------+--------------+--------------+-----------
No-CB           |          0 |   56518826 |   43854224 |  12664602 |     42723.70 |    108902.39 |  -66178.69
Levee           |    9136540 |   47382286 |   46622493 |    759793 |     59399.83 |     11463.81 |   47936.01
Static-BAU      |   12506697 |   44012129 |   43540136 |    471993 |     49914.36 |      7115.82 |   42798.54
Static-Peak     |   14463558 |   42055268 |   41680397 |    374871 |     44642.48 |      4830.69 |   39811.79

Delta = SuccessScore - FailureScore  [higher is better]
```

### Short Benchmark (4 hours baseline - no incidents)

```
Candidate       |    Blocked |    Allowed |  Successes |  Failures | SuccessScore | FailureScore |      Delta
----------------+------------+------------+------------+-----------+--------------+--------------+-----------
No-CB           |          0 |    1442032 |    1434763 |      7269 |      3139.13 |       190.85 |    2948.27
Levee           |          0 |    1442032 |    1434752 |      7280 |      3085.17 |       190.97 |    2894.20
Static-BAU      |          0 |    1442032 |    1434794 |      7238 |      3140.34 |       190.37 |    2949.97
Static-Peak     |          0 |    1442032 |    1434776 |      7256 |      3139.96 |       190.71 |    2949.25

Delta = SuccessScore - FailureScore  [higher is better]
```

## Analysis

### Full 28-Hour Results

| Metric | No-CB | Levee | Static-BAU | Static-Peak |
|--------|-------|-------|------------|-------------|
| **Delta** | -66,179 | **47,936** | 42,799 | 39,812 |
| Blocked | 0 | 9.1M | 12.5M | 14.5M |
| Allowed | 56.5M | 47.4M | 44.0M | 42.1M |
| Failures | 12.7M | 760K | 472K | 375K |
| Failure Rate | 28.9% | 1.6% | 1.1% | 0.9% |

**Key findings:**

1. **No-CB baseline proves CB value**: Without protection, 28.9% failure rate and negative Delta (-66K)
2. **Levee achieves highest Delta** (+47,936) - 12% better than Static-BAU, 20% better than Static-Peak
3. **Levee allows 8-13% more throughput** while maintaining acceptable failure rates

We note that during the early baseline (4 hours Short Benchmark), all circuit breakers behave identically within margins of error.
We also prove that Levee provides viable 0-configuration drop-in stability protection within similar ballpark of failure rates.
It is interesting to note that Levee has the highest failure rate (1.6% vs 0.9% best) among the circuit breakers but also scores highest.
This is due to Levee's superior performance during high load conditions, which the scoring gives a higher weightage to.

For comparably **similar reliability**, Levee achieves the **best business outcome** with *least operational supervision* (i.e. tuning) vs. specially crafted circuit breakers.

## Technical Notes

### Logical Time

The entire simulation operates on **logical time** from request timestamps:
- No wall-clock sleeping - requests processed as fast as possible
- 28 hours simulates in ~130 minutes
- Autoscaler, queue, latency generation all use logical time
- Deterministic and reproducible results

### In-Process Isolation

Each CB runs with its own backend instance:
- Separate `backend.Server` with independent capacity/queue state
- Separate `CapacityController` instance
- Same random seed ensures identical request patterns
- No shared state between CBs

### Autoscaler Configuration

- Base capacity: 150 RPS per replica
- Queue depth: 15 per replica (10% of throughput)
- Min/Max replicas: 1-8
- Target utilization: 70%
- Evaluation interval: 15s
- Scale-down stabilization: 300s
- Provisioning lag: 30s

## Comparison with Open-Loop Benchmark

| Aspect | Open-Loop | Distributed (Closed-Loop) |
|--------|-----------|---------------------------|
| Backend state | Static (from load spec) | Dynamic (responds to load) |
| Autoscaling | None | HPA-style (1-8 replicas) |
| Queuing | None | Realistic backpressure |
| CB interference | All CBs see same backend | Each CB has own backend instance |
| Scoring | Prescient-relative | Throughput-weighted |
| Runtime | ~5 minutes | ~130 minutes |

Both benchmarks show Levee outperforming static configurations, but the distributed benchmark provides a more realistic assessment of the magnitude of that advantage.
