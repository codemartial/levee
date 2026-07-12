# Adversarial test findings

These results come from deterministic synthetic traces in this directory. They
characterize the current controller under the stated workloads; they are not a
claim that the same timings or rejection counts apply to every production
system.

## Low request rates

A healthy 0.1 RPS stream remained closed across 200 requests. A cold outage
throttled after five consecutive failures at both tested rates, but those five
samples took 40 seconds at 0.1 RPS and 400 milliseconds at 10 RPS.

Finding: cold-start detection is sample-driven. The consecutive-failure path
provides a deterministic sample bound, but wall-clock response necessarily
stretches with sparse traffic.

## Mixed request classes

The trace combined a healthy 100 RPS/5ms class with a failing 20 RPS/50ms class.
One shared Levee rejected 990 healthy fast-class calls. With one Levee per
class, the fast class stayed closed and rejected none; the slow class was
contained independently.

Finding: materially different reliability/latency classes should not share a
Levee when preserving the healthy class matters. A shared error estimate and cap
cannot isolate them.

## Correlated failures and retries

After healthy warm-up, dispersed 20% failures with no consecutive failures took
156 physical attempts to throttle on statistical evidence. A correlated run
tripped after seven consecutive failures. A retry-amplified sequence also
needed seven physical failed attempts, which is roughly three logical calls
when each can make two retries.

Finding: correlation and retries make the consecutive-failure path dominate,
so protection arrives much sooner than an independence-flavoured statistical
interpretation would suggest. Retry attempts consume the evidence budget as
real admitted attempts.

## Proactive surge behavior

- With the same learned onset of 15, stalled load at roughly 2x onset tripped in
  42ms; roughly 4x onset tripped in 26ms.
- Draining 17 armed operations successfully relaxed strain to zero without a
  false trip.
- A genuinely healthy capacity step from 100 RPS to 2000 RPS at constant 10ms
  latency caused no throttling or rejection. The EWMA concurrency estimate rose
  from 1.00 to 8.03 during the two-second step; healthy completion proof kept
  the surge mechanism from mistaking the new regime for congestion while the
  estimate caught up.

Finding: surge urgency is monotonic in overload severity in these traces, strain
relaxes under healthy drain, and completion proof successfully distinguishes a
healthy capacity increase from stalled inflight. The EWMA estimate intentionally
lags a sharp capacity step.

## Seeded invariant trace

A fixed-seed 25,000-arrival trace mixed healthy traffic, multimodal latency,
correlated failures, retry-like arrival density, long calls, overload, and
recovery. It admitted 11,577 calls and rejected 13,423. It exercised statistical
failure, consecutive-failure, open-at-minimum-limit, cooldown-probe, and recovery
transitions.

No illegal state transition, missing/spurious trigger, inflight accounting
mismatch, invalid active cap, negative inflight value, non-finite diagnostic, or
admitted/completed conservation error was observed.

## Deliberate exclusions

Long-lived/streaming operations and workload-class switching are outside Levee's
design envelope. The suite does not attempt to train on one workload class and
then characterize controller behavior after changing that class.

The suite also does not define caller cancellation, hedging, duplicate/missing
completion, outcome classification, or out-of-order timestamp semantics. Those
remain caller/API concerns outside the agreed scope. Explicit timestamps appear
only to make the test traces deterministic.
