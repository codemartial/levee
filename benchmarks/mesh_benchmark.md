# Mesh Benchmark: Levee in Mesh Mode

This benchmark answers one question: can many interacting Levee instances
govern a service mesh without destructive interactions -- synchronized
flapping, throttle cascades, oscillation -- while containing overload better
than a conventional static stack?

Every node replica runs its own in-process Levee instances: one for inbound
admission and one per outbound edge, scaling horizontally with the node. The
competitors are conventional hand-configured guardrail stacks in three
combinations -- per-edge static circuit breakers only, static rate +
max-inflight limiters only, and both chained -- each sized from the node's
provisioning profile the way a careful operator would size them. A
no-governor control run shows what the mesh does with no protection at all.

## Topology: "Storefront" (10 nodes)

```
 edge-api (entry, 300 RPS) --0.80--> catalog --1.0--> pricing --1.0--> db
     |                                  |--0.50--> inventory --1.0--> db
     |--0.25--> orders --1.0--> inventory
     |               |--1.0--> payments --1.0--> db
     |                              |~~1.0~~> notify ~~0.30~~> orders (cycle)
     |~~0.02~~> audit
 admin-api (entry, 20 RPS) --0.50--> catalog
     |--0.50--> inventory

 --p-->  sync call with probability p
 ~~p~~>  async fire-and-forget (callback) with probability p
```

| Node | Role | Entry RPS | RPS/replica | Min/Max repl | P50/P99 ms | Steady RPS |
|---|---|---|---|---|---|---|
| edge-api | entry (high) | 300 | 100 | 2/16 | 5/20 | 300 |
| admin-api | entry (low) | 20 | 25 | 1/4 | 10/40 | 20 |
| catalog | mid | - | 100 | 2/12 | 10/40 | 250 |
| pricing | mid | - | 100 | 2/12 | 8/30 | 250 |
| orders | mid (cycle) | - | 50 | 2/12 | 15/60 | 107 |
| inventory | fan-in | - | 100 | 2/12 | 10/40 | 242 |
| payments | branch | - | 50 | 2/12 | 20/80 | 107 |
| db | deep shared fan-in | - | 200 | 3/16 | 5/25 | 599 |
| notify | async hub | - | 50 | 2/12 | 10/40 | 107 |
| audit | low-throughput leaf | - | 10 | 1/2 | 20/100 | 6 |

Design rules, enforced by `Topology.Validate()` (unit-tested):

- Steady-state inbound RPS fits each node's capacity envelope with headroom.
- A high-throughput caller may reach a low-throughput callee only through a
  throughput divider (edge-api -> audit at p = 0.02).
- Sync edges form a DAG; cycles exist only over async callback edges
  (orders -> payments -> notify -> orders), and every request carries a hop
  budget of 8 so callback loops terminate.

The cycle has async gain 0.3, so orders' steady inbound is 75/(1-0.3) = 107
RPS. Hop latency is a constant 1 ms. Root requests time out end-to-end at
1500 ms; a timeout is the only signal an upstream ever gets about a crashed
downstream.

## Node model

Each node is an elastic-capacity processor:

- Capacity scales in replica multiples via the same HPA-style
  `CapacityController` as the distributed benchmark: 70% target utilization,
  15s evaluation, 30s provisioning lag. Scale-up decisions are immediate but
  capacity arrives 30s late. The HPA only sees admitted traffic.
- Nodes warm-start at their design replica count with the HPA's demand
  estimator seeded, modeling a mesh already in steady operation.
- Worker concurrency per replica is sized by Little's Law from the node's
  mean latency. Queue wait emerges from slot contention (FIFO).
- The node buffers up to 3 seconds' worth of demand at instantaneous
  capacity (3 * perReplicaRPS * replicas requests, executing + queued).
  Hitting the buffer limit is an immediate CRASH (queue saturation kills
  every instance in the deployment), not a shed.
- A crash drops all in-flight work and starts 120s of downtime; the node
  then restarts at MinReplicas while the HPA remembers pre-crash demand
  and re-scales.
- During downtime, arriving calls sit unanswered and fail at their root
  deadline. Upstreams observe only timeouts, never the downtime itself.
- Physical occupancy is decoupled from logical outcomes: work whose root has
  already timed out keeps its slot until scheduled completion (work
  amplification). Deadline propagation stops post-timeout child dispatch.

## Governor compositions

Governors are horizontally replicated like the nodes they run on: each node
replica is a separate process with its own in-process governor instances,
and admission round-robins across them (the load balancer). Instances start
at the node's design replica count (warm start), scale-up adds cold
instances, and a crash kills all instances; recovery starts fresh ones
at MinReplicas. The request's admitting replica also makes that request's
downstream calls through its own client-side instances.

- **Levee**: per replica, one instance inbound + one per outbound edge, all
  with the same SLO (90% success, 1500 ms), matching the distributed suite.
  The inbound instance records the outcome the node returns: local work AND
  its immediate sync downstream calls (by transitivity, the subtree
  outcome). This is what lets an entry node throttle for trouble three hops
  down.
- **Static candidates**: tuned from the provisioning profile alone --
  initial scale and max capacity, plus the safety margins an operator would
  bake in. No scenario foreknowledge: no entry rates, no surge multipliers,
  no solved steady-state traffic. Sizing derivations are in
  `mesh/static.go`. Three combinations run:
  - **Static-Breaker**: per replica and per edge, the distributed suite's
    `StaticCB`. Config selection follows the documented derivation ranges
    in `static_cb.go` by the per-instance volume each breaker can see,
    which is bounded by the replica's own service ceiling: BAU up to 800
    RPS, Peak above (at per-replica ceilings of <= 200 RPS, every mesh
    edge gets BAU). Client-side only; no inbound protection.
  - **Static-Limiter**: per-replica inbound token bucket at 85% of the
    replica's service ceiling (0.85 * PerReplicaRPS, burst = 1s of
    tokens) -- local rate limiting, the standard per-pod deployment.
    Aggregate admission is replicas * 85% of replica capacity, so it
    tracks the live fleet: tight while the autoscaler lags, wide once
    capacity has actually arrived, never past what the current fleet can
    serve. Plus per replica and per edge a max-inflight limiter at
    ceil(max(1.5 * m, m + 3 * sqrt(m))), where m = p_edge *
    PerReplicaRPS * subtreeMeanLatency is the mean inflight of a replica
    driving the edge at its own service ceiling and the 3-sigma term
    covers Poisson burstiness (dominant at these small means). Both are
    outcome-blind.
  - **Static-Full**: both of the above, chained. The breaker consults
    `Start` before the limiter so its OPEN -> HALF_OPEN clock always
    advances; a limiter rejection cancels the admission so probe slots
    are not leaked. Chaining retunes the breakers on interaction
    principles: the limiter already bounds every congestion mode, and
    tripping a sync edge fails the parent outright while serving it at
    any success rate s > 0 gives the parent chance s -- so sync-edge
    breakers narrow to dead-edge detection (trip on 20 consecutive
    failures: needs >= 2.1M calls at any partial failure rate p <= 0.5,
    ~20-80 calls at p >= 0.9). Async edges keep the aggressive BAU
    config, because a dropped callback never fails its parent and false
    trips only shed optional work.
- **No-Gov**: admits everything.

Because the per-replica limits track the live fleet, the static limiter
stack has an answer for both surges on paper: the servable x4 surge
(phase B) passes progressively as autoscaling delivers replicas, and the
x8 overcap flood (phase F), unservable at any scale, is clipped at
whatever the current fleet can serve. What no static limit can know is
whether the work it admits will complete -- that distinction only
appears in outcomes, and it decides the degradation phases.

## Scenario (30 simulated minutes)

| Phase | Window | Injection | What it probes |
|---|---|---|---|
| A steady | 0-180s | none | convergence; no spurious throttling |
| B surge | 180-330s | edge-api arrivals x4, 5s ramp | fan-out amplification vs 30s scale lag |
| B settle | 330-420s | none | return toward steady |
| C degrade | 420-570s | db: latency x4, +15% errors | fan-in backpressure without cascade |
| C settle | 570-660s | none | recovery speed (3s memory half-life) |
| D crash | 660-780s | payments: latency x8 | branch crash; 120s recovery shielding |
| E recovery | 780-1200s | none | return to CLOSED, no flapping |
| F overcap | 1200-1350s | edge-api arrivals x8, 5s ramp | sustained load beyond MaxReplicas capacity |
| F settle | 1350-1440s | none | return toward steady |
| G bugstorm | 1440-1590s | catalog->inventory calls x10 | interior code-bug surge invisible to entries |
| H recovery | 1590-1800s | none | full-run recovery, no residual flapping |

The phase-D injection cuts payments' effective throughput ~8x below its
inbound rate; its queue saturates and it crashes within ~15s unless
upstream governors shed load. Levee's ~3s EWMA half-life lets settle windows
decay most short-lived memory, reducing phase carryover.

Phase F is qualitatively different from phase B: at x8, edge-api's inbound
(2400 RPS) exceeds even its fully scaled capacity (16 replicas x 100 RPS),
so no amount of autoscaling absorbs it; the mesh must shed most of the
entry stream for 150 straight seconds or crash. Phase G models a shipped
code bug (cache bypass plus retries): each catalog request makes ~10
inventory calls instead of ~0.5, driving inventory to ~1370 RPS against its
1200 RPS replica ceiling. The stressor originates mid-mesh, so entry
governors can see it only through subtree outcomes.

Both surge phases ramp linearly over 5 seconds rather than stepping
instantaneously: an origin behind an edge network never sees a 0 ms step,
because user arrivals and CDN/LB connection pools spread an onset over
seconds. The ramp width interacts with the feedback physics. The earliest
outcome signal a feedback governor can observe is a root timeout at 1.5 s,
and at x8 the 5 s onset fills the 3 s entry buffer roughly a second after
that first signal -- too late to shed inflow below capacity. Phase F is
therefore beyond the reaction window of outcome feedback by construction:
a governor that waits for failure evidence cannot prevent the entry crash.
Admission-side congestion signals (inflight climbing past the healthy
operating point) are available within the first few hundred milliseconds
of the onset, so in practice the phase separates governors by which signal
they act on.

Injections are non-overlapping, so each phase attributes any regression to
exactly one stressor. Compound-failure scenarios (surge during degradation)
are out of scope.

## What is measured

- **Per-role state dwell** (diagnostic): for each Levee governor role with
  non-CLOSED time, the % of its lifetime spent THROTTLED and OPEN plus the
  state-transition count. This shows where and how shedding happened, and
  the transition counts are the flapping detector.
- **Delta scores**: the suite-standard throughput-weighted epoch score,
  Delta = (SuccessScore - FailureScore) * Allowed / (Allowed + Blocked),
  computed per entry point on end-to-end outcomes and for the combined mesh
  stream. Entry requests arriving during entry-node downtime count as
  allowed failures (nothing admitted or blocked them).
- **Node crashes**: per-candidate crash counts and per-node downtime%,
  reported as a diagnostic listing.

## Determinism and comparability

Everything runs on synthetic time in a single-threaded discrete-event loop
per candidate; same seed gives bit-identical results. Per-call randomness
(service latency, error rolls, edge rolls) is hash-derived from
candidate-independent call IDs, and entry arrivals come from dedicated PCG
streams, so every candidate sees identical entry arrivals and identical nominal
draws for corresponding calls; queue waits, downstream calls that survive
admission, and observed outcomes differ by candidate.

## Results (seed 20241225, full scenario)

```
Candidate       |   Allowed |   Blocked |   Success |  Failures |  MeshDelta
No-Gov          |   1018091 |         0 |    242983 |    775108 |   -25256.5
Static-Breaker  |   1018091 |         0 |    274045 |    744046 |   -24511.7
Static-Limiter  |    675086 |    343005 |    599082 |     76004 |     8141.9
Static-Full     |    675086 |    343005 |    596854 |     78232 |     7929.6
Levee           |    639035 |    379056 |    592268 |     46767 |     8587.3

Crashes: No-Gov: edge-api x7, inventory x2, db x2, payments x1.
         Static-Breaker: edge-api x7, inventory x2.
         Static-Limiter: none. Static-Full: none. Levee: none.

Per-entry Delta: Levee edge-api=8103.2 admin-api=670.4;
                 Static-Limiter edge-api=7591.1 admin-api=715.2.
```

How each candidate fares:

- **No-Gov** crashes twelve times: edge-api twice in the surge and twice
  in the overcap, db twice under degradation, payments in phase D, and
  inventory twice in the bugstorm. It also exposes a recovery trap: a
  crashed entry restarts at MinReplicas into full unshed traffic, refills
  its 3s buffer before re-scaled capacity arrives (30s provisioning lag),
  and crashes again -- three more edge-api crashes ride that loop after
  the overcap ends, at plain steady load, for 44% entry downtime.
- **Static-Breaker** repeats all seven of No-Gov's edge-api crashes --
  client-side breakers provide no inbound protection, so both surges
  saturate the entry queue unopposed. Where failures do appear on
  outbound edges it helps: breakers on the db- and payments-facing edges
  trip during the degradation and branch-crash phases, avoiding No-Gov's
  db and payments crashes. The bugstorm still fells inventory twice,
  because the amplified catalog->inventory calls mostly succeed until
  the moment the queue saturates -- a consecutive-failure counter cannot
  see success-dominated overload coming. With the entry down 44% of the
  run, none of that moves the score.
- **Static-Limiter** is the genuine competitor: zero crashes anywhere,
  the most raw successes of any candidate (599,082, edging out Levee's
  592,268), and a MeshDelta within 5.5% of Levee's. Per-replica buckets
  shed inflow to live capacity through both surges -- serving the x4
  surge progressively as autoscaling delivers replicas -- and the
  max-inflight caps bind when a callee's latency inflates (C, D) or
  calls-per-request multiply (G). Its one blind spot is the whole gap:
  it prices volume, not viability. During the degradation phases it
  keeps admitting its capacity's worth of traffic into subtrees where
  that work goes to die, finishing with 1.6x Levee's failures (76,004
  vs 46,767) -- and under throughput-weighted scoring, that failure gap
  is the entire lead Levee keeps.
- **Static-Full** shows that in a chained stack the breaker's optimum is
  to approach inertness. With BAU breakers on sync edges it loses 2,298
  MeshDelta to limiter-only: the inflight caps already bound every
  overload the breakers could catch, and a tripped sync-edge breaker
  fails the parent request outright, so each 10s OPEN window during the
  degradation phases converts traffic the limiter would have served --
  degraded but mostly completing -- into guaranteed failures. Retuning
  the breakers for the chained role (sync edges as dead-edge detectors,
  async edges aggressive) recovers 91% of that: the dead-edge config
  stays fully closed through the db degradation and the payments crash,
  and the async notify->orders breaker parks OPEN through stress
  windows, shedding only optional callbacks. The residual -212 comes
  from the one place the dead-edge heuristic still misfires: during the
  overcap flood, catalog's own inbound buckets reject edge-api's
  admitted overflow in sustained bursts, and a consecutive-failure
  counter cannot tell that rejection storm from a dead edge. Even
  retuned on pure interaction principles, the breaker adds nothing the
  limiter does not already bound -- Static-Full converges to
  Static-Limiter from below.
- **Levee** posts the top MeshDelta, the fewest failures, and zero
  crashes. It wins every phase that feedback can decide: it serves the
  full planned surge with no crash and no clamp (B), throttles around the
  degraded db (C), and sheds the dying payments branch at the entries
  (D). In the two phases built to outrun outcome feedback it trips on
  congestion instead: the overcap flood (F) and the bugstorm's amplified
  inventory traffic (G) are both shed before the first timeout can
  report. Against the strongest static it converts admitted work at
  92.7% vs 88.7% -- feedback spends the same admission budget on
  requests that can actually complete.

Phase F is decided by which signal a governor acts on. The first outcome
signal -- a root timeout at 1.5s -- arrives about a second before the
entry buffer saturates: too late, as the seven edge-api crashes of
No-Gov and Static-Breaker attest. The limiter stacks survive it by
arithmetic: per-replica buckets clip the flood at what the live fleet
serves, no signal required. Levee survives it by feedback that outruns
outcomes. While uncapped it publishes a stretch onset just past the
statistical noise of its Little's-law healthy operating point (goodput
x latency); the x8 flood crosses that within ~100ms of onset and starts
loading the surge spring, at a rate proportional to how far past health
the flood stretches. Completions could relax the spring by proving the
new concurrency healthy (completion rate x pre-surge latency, Little's
law again), but a real flood proves nothing -- its completions show
rate pinned at capacity -- so the strain budget is spent in a fraction
of an evaluation window and admission snaps to the proven capacity. The
unservable load is shed as Blocked instead of queueing into a crash,
and the autoscaler keeps scaling on the admitted stream. Under
sustained overcap, the strain stays loaded across each trip, so
recovery probes that re-breach re-trip immediately and spill only a
bounded burst. Where the static arithmetic and the feedback part ways
is the degradation phases: a bucket sized to capacity keeps admitting
full volume into a subtree that can no longer complete it, while
subtree outcomes pull Levee's admission down to what remains viable.
That difference -- 29,237 failures -- is the margin.

Where Levee's adaptivity shows is in the phases that require judgment
rather than a fixed cap. During the planned surge, entry and downstream
instances enter THROTTLED rather than flipping fully OPEN, keeping
capacity-matched work flowing while the autoscaler catches up. When db
degrades, the db-facing roles throttle without forcing unrelated edges to
stop, preserving sibling traffic that can still complete. When the
payments branch crashes, failures surface through subtree outcomes at the
entry nodes, so admission shifts toward the point of origin. And in the
bugstorm, catalog's amplified inventory calls are shed at the
catalog->inventory edge while inventory's own inbound instances trip on
the congestion, keeping it up under an injection that fells it twice
without governance.

Two more mesh-mode behaviors make that work without coordination. The async
callback edge (notify -> orders) parks OPEN/THROTTLED during stress, shedding
optional work first because callbacks never affect their parent's outcome.
And the server-side inbound instances provide a second line of defence under
fan-in that does not depend on how many callers or replicas happen to be
active. Per-replica instances each adapt on 1/R of a node's traffic, so
high-replica nodes converge a little slower than a single shared instance
would.

## Model scope

The scenario is about overload, degradation, crash recovery, and
scale-up lag. Scale-down plays a minor role: the capacity controller uses a
600s scale-down delay, so replicas that arrive during an incident remain
available through the following settle windows, though capacity added early
in the run may begin draining in the back half of the 30-minute timeline.

Async edges model best-effort RPC callbacks (webhook-style): a rejected
dispatch is silently dropped and a callback lives or dies within one root
timeout. Durable-queue notification paths, which convert overload into
delay instead of loss, are out of scope. Governors see async calls exactly
as they see sync calls -- same admission decisions, same Success/Fail
feedback -- only the parent's indifference to the outcome differs.

## Run it

From the `benchmarks/` directory:

```
# Full 30-minute scenario, a few seconds wall per candidate
go test -v -run '^TestMeshBenchmark$' -timeout 10m

# Phases A+B only, for quick iteration
go test -v -run TestMeshBenchmarkShort -timeout 5m

# Engine and model unit tests
go test ./mesh/
```
