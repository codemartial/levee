# Mesh Benchmark: Levee in Mesh Mode

This benchmark answers one question: can many interacting Levee instances
govern a service mesh without destructive interactions -- synchronized
flapping, throttle cascades, oscillation -- while containing overload better
than a conventional static stack?

Every node replica runs its own in-process Levee instances: one for inbound
admission and one per outbound edge, scaling horizontally with the node. The
competitor stack is a conventional hand-configured guardrail setup: a
node-level static token-bucket rate limiter inbound, plus per-replica static
circuit breakers chained with static max-inflight concurrency limiters per
outbound edge. A no-governor control run shows what the mesh does with no
protection at all.

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
downstream calls through its own client-side instances. The one exception is
the static inbound rate limiter, which stays single-per-node (distributed
rate limiting).

- **Levee**: per replica, one instance inbound + one per outbound edge, all
  with the same SLO (90% success, 1500 ms), matching the distributed suite.
  The inbound instance records the outcome the node returns: local work AND
  its immediate sync downstream calls (by transitivity, the subtree
  outcome). This is what lets an entry node throttle for trouble three hops
  down.
- **Static-Nominal**: node-level inbound token bucket at 1.5x steady-state
  design RPS (burst = 1s of tokens); per replica and per edge a max-inflight
  limiter at ceil(max(1.5 * m, m + 3 * sqrt(m))), where m = edgeRPS *
  subtreeMeanLatency / designReplicas is the mean per-instance inflight and
  the 3-sigma term covers Poisson burstiness (dominant at these small
  means), chained with the distributed suite's `StaticCB`. The breaker
  consults `Start` before the limiter so its OPEN -> HALF_OPEN clock always
  advances; a limiter rejection cancels the admission so probe slots are
  not leaked. Config
  selection follows the documented derivation ranges in `static_cb.go` by
  the per-instance volume each breaker actually sees: BAU up to 800 RPS,
  Peak above (at realistic per-pod volumes, every mesh edge gets BAU).
  Sizing derivations are in `mesh/static.go`. The rate limiter is
  outcome-blind.
- **Static-Peak**: same formulas with the 4x surge folded into the sizing,
  i.e. tuned generously enough to admit the whole planned peak. The x8
  overcapacity surge (phase F) is deliberately beyond what any operator
  would provision static configs for; it is unplanned load for every
  candidate.
- **No-Gov**: admits everything.

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
Levee           |    639035 |    379056 |    592268 |     46767 |     8587.3
Static-Nominal  |    622450 |    395641 |    505943 |    116507 |     5005.3
Static-Peak     |   1018091 |         0 |    273586 |    744505 |   -24643.3

Crashes: No-Gov: edge-api x7, inventory x2, db x2, payments x1.
         Static-Peak: edge-api x7. Levee: none. Static-Nominal: none.

Per-entry Delta: Levee edge-api=8103.2 admin-api=670.4;
                 Static-Nominal edge-api=4534.8 admin-api=643.5.
```

How each candidate fares:

- **No-Gov** crashes twelve times: edge-api twice in the surge and twice
  in the overcap, db twice under degradation, payments in phase D, and
  inventory twice in the bugstorm. It also exposes a recovery trap: a
  crashed entry restarts at MinReplicas into full unshed traffic, refills
  its 3s buffer before re-scaled capacity arrives (30s provisioning lag),
  and crashes again -- three more edge-api crashes ride that loop after
  the overcap ends, at plain steady load, for 44% entry downtime.
- **Static-Peak** is sized to admit the whole planned x4 peak, so both
  surges pour straight into the entry queue and it matches No-Gov's seven
  edge-api crashes. A rate limiter tuned for the peak does not prevent
  queue saturation while autoscaling lags; it only prevents shedding.
- **Static-Nominal** never crashes anything by clamping the mesh to 1.5x
  steady state, forfeiting both surges wholesale -- including phase-B
  traffic the mesh demonstrably had capacity to serve. The clamp keeps
  its score positive, but it trails Levee on successes and MeshDelta
  alike: blind pre-provisioning buys crash immunity at the price of every
  servable surge it refuses.
- **Levee** posts the top MeshDelta, the most successes, and zero
  crashes. It wins every phase that feedback can decide: it serves the
  full planned surge with no crash and no clamp (B), throttles around the
  degraded db (C), and sheds the dying payments branch at the entries
  (D). In the two phases built to outrun outcome feedback it trips on
  congestion instead: the overcap flood (F) and the bugstorm's amplified
  inventory traffic (G) are both shed before the first timeout can
  report, so edge-api and inventory stay up through injections that
  crash them under every non-clamped alternative.

Phase F is decided by which signal a governor acts on. The first outcome
signal -- a root timeout at 1.5s -- arrives about a second before the
entry buffer saturates: too late, as No-Gov's and Static-Peak's seven
edge-api crashes attest. Levee does not wait for outcomes. While
uncapped it publishes a stretch onset just past the statistical noise
of its Little's-law healthy operating point (goodput x latency); the
x8 flood crosses that within ~100ms of onset and starts loading the
surge spring, at a rate proportional to how far past health the flood
stretches. Completions could relax the spring by proving the new
concurrency healthy (completion rate x pre-surge latency, Little's law
again), but a real flood proves nothing -- its completions show rate
pinned at capacity -- so the strain budget is spent in a fraction of
an evaluation window and admission snaps to the proven capacity. The
unservable load is shed as Blocked instead of queueing into a crash,
and the autoscaler keeps scaling on the admitted stream. Under
sustained overcap, the strain stays loaded across each trip, so
recovery probes that re-breach re-trip immediately and spill only a
bounded burst. Static-Nominal survives the
same phase by never observing anything; that same blindness is why it
forfeits the servable phase-B surge wholesale.

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
