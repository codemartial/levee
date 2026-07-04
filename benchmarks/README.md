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

- **[Mesh benchmark](mesh_benchmark.md)** -- a 10-node service mesh where every node
  runs Levee for both inbound and outbound admission, against a static rate limiter +
  circuit breaker + concurrency limiter stack. Through a surge, deep-dependency
  degradation, and a branch crash, Levee is the only candidate with zero node
  crashes and a positive score.

All suites run on logical time, so results are deterministic and reproducible: no
wall-clock sleeps, no flakiness.

## Headline results

Across a simulated 28-hour Cyber Monday, versus carefully hand-tuned static breakers:

- **Wins 20/20** configurations tested -- every load variation, SLO, and queue depth.
- **Zero backend crashes** in the reference run, where the best-tuned static breaker
  crashed the backend 50 times -- and up to **10x fewer crashes** at the most
  crash-prone queue depth (8 vs 85).
- **Up to 67% more throughput** admitted under extreme overload, where a binary
  open/close cannot match admission to capacity.

Full tables and methodology in [distributed_benchmark.md](distributed_benchmark.md).

## Try it

From this directory:

```
# Quick: a single incident, finishes in minutes
go test -v -run TestDistributedBenchmarkFirstIncident -timeout 15m

# Full distributed run
go test -v -run '^TestDistributedBenchmark$' -timeout 60m
```

Bring your own static breaker config and see how it stacks up.
