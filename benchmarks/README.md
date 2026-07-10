# Levee Benchmarks

Levee tunes itself -- these benchmarks prove it works. This directory holds three
complementary simulation suites that put self-tuning Levee head-to-head with
carefully hand-tuned static circuit breakers.

- **[Open-loop benchmark](benchmark.md)** -- replays a deterministic synthetic
  workload against every breaker at once and scores each decision against an oracle
  that knows the future. Pure decision quality, no feedback.

- **[Distributed benchmark](distributed_benchmark.md)** -- a closed-loop simulation
  where breaker decisions move backend load, autoscaling, queue backpressure, and
  node crashes. Levee wins every load variation, by the widest margins under extreme
  overload.

- **[Mesh benchmark](mesh_benchmark.md)** -- a 10-node service mesh with hundreds
  of live Levee instances, where every replica runs Levee for inbound and
  per-edge outbound admission against capacity-sized static stacks: circuit
  breakers only, rate + concurrency limiters only, and the two combined.
  Through a planned surge, deep-dependency degradation, a branch crash, an
  arrival flood beyond maximum autoscaled capacity, and an interior
  call-amplification bug storm, Levee posts the top score with zero node
  crashes.

The closed-loop suites (distributed and mesh) run on logical time with fixed
seeds, so their results are bit-reproducible. The open-loop suite's decision
counts are seeded and reproducible too.

## Headline results

### Mesh suite

In the 10-node, 30-minute mesh scenario, where hundreds of Levee instances
operate without shared state:

- **Top mesh score, fewest failures, zero node crashes**: Levee posts +8,587
  MeshDelta against +8,142 for the strongest static stack (per-replica rate
  limiters + max-inflight caps, which also survives crash-free). The margin
  is failure discrimination: capacity-priced admission keeps feeding
  degraded subtrees, ending with 1.6x Levee's failures; Levee converts
  admitted work at 92.7% vs 88.7%. Breakers fare worse: alone they crash
  the entry seven times (no inbound protection), and stacked on limiters
  they only subtract -- even retuned to trip solely on dead edges, the
  combined stack converges to limiter-only from below (+7,930).
- **Sheds arrival floods before the first failure exists**: the overcap phase
  drives 8x load past maximum autoscaled capacity with an onset faster than
  outcome feedback; Levee's surge trip fires on congestion (inflight vs its
  Little's-law healthy point) within ~100ms and keeps the entry up.
- **Origin-side protection emerges from mesh operation**: because inbound
  Levee instances record subtree outcomes, failures several hops down push
  entry points to shed work before downstream service time and queue space are
  spent on requests that cannot complete.

Full tables and methodology in [mesh_benchmark.md](mesh_benchmark.md).

### Distributed suite

Across a simulated 28-hour Cyber Monday, versus carefully hand-tuned static breakers:

- **Wins 21/21** recorded configurations across all suites -- every load
  variation, SLO, and queue depth, plus the isolated, cooperative, prescient,
  and mesh runs.
- **Zero backend crashes** in the reference run and at every queue depth up to
  1.4x, where the best-tuned static breaker crashed the backend 50-65 times --
  and **9x fewer crashes** at the most crash-prone depth (9 vs 85).
- **Up to 61% more throughput** admitted under extreme overload, where a binary
  open/close cannot match admission to capacity.

Full tables and methodology in [distributed_benchmark.md](distributed_benchmark.md).

## Try it

From this directory:

```
# Quick: a single incident, finishes in minutes
go test -v -run TestDistributedBenchmarkFirstIncident -timeout 15m

# Full distributed run
go test -v -run '^TestDistributedBenchmark$' -timeout 60m

# Full mesh run
go test -v -run '^TestMeshBenchmark$' -timeout 10m
```

Bring your own static breaker config and see how it stacks up.
