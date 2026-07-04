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
- Worker concurrency per replica is sized by Little's Law from the node's
  mean latency. Queue wait emerges from slot contention (FIFO).
- The node buffers up to 5 seconds' worth of demand at instantaneous
  capacity (5 * perReplicaRPS * replicas requests, executing + queued).
  Hitting the buffer limit is an immediate CRASH, not a shed.
- A crash drops all in-flight work. Recovery takes 120s; the node restarts
  at MinReplicas while the HPA remembers pre-crash demand and re-scales.
- While crashed, arriving calls sit unanswered and fail at their root
  deadline. Upstreams observe only timeouts, never the crash.
- Physical occupancy is decoupled from logical outcomes: work whose root has
  already timed out keeps its slot until scheduled completion (work
  amplification). Deadline propagation stops post-timeout child dispatch.

## Governor compositions

Governors are horizontally replicated like the nodes they run on: each node
replica is a separate process with its own in-process governor instances,
and admission round-robins across them (the load balancer). Scale-up adds
cold instances; a crash kills all instances and recovery starts fresh ones
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
  limiter at ceil(1.5 * edgeRPS * subtreeMeanLatency / designReplicas),
  chained with the existing `StaticCB`. The breaker consults `Start` before
  the limiter so its OPEN -> HALF_OPEN clock always advances; a limiter
  rejection cancels the admission so probe slots are not leaked. Config
  selection follows the documented derivation ranges in `static_cb.go` by
  the per-instance volume each breaker actually sees: BAU up to 800 RPS,
  Peak above (at realistic per-pod volumes, every mesh edge gets BAU).
  Sizing derivations are in `mesh/static.go`. The rate limiter is
  outcome-blind.
- **Static-Peak**: same formulas with the 4x surge folded into the sizing,
  i.e. tuned generously enough to admit the whole surge.
- **No-Gov**: admits everything.

## Scenario (20 simulated minutes, 5 phases)

| Phase | Window | Injection | What it probes |
|---|---|---|---|
| A steady | 0-180s | none | convergence; no spurious throttling |
| B surge | 180-330s | edge-api arrivals x4 | fan-out amplification vs 30s scale lag |
| B settle | 330-420s | none | return toward steady |
| C degrade | 420-570s | db: latency x4, +15% errors | fan-in backpressure without cascade |
| C settle | 570-660s | none | recovery speed (3s memory half-life) |
| D crash | 660-780s | payments: latency x8 | branch crash; 120s recovery shielding |
| E recovery | 780-1200s | none | return to CLOSED, no flapping |

The phase-D injection collapses payments' effective throughput ~8x below its
inbound rate; its queue saturates and it crashes within ~15s unless upstream
governors shed load. Levee's ~3s EWMA half-life lets settle windows decay most
short-lived memory, reducing phase carryover.

## What is measured

- **State dwell**: duration-weighted % of governor-role lifetime in
  CLOSED / THROTTLED / OPEN(+HALF_OPEN). For the static stack, limiter
  saturation is reported as THROTTLED and breaker-open as OPEN, so heavy
  static shedding cannot masquerade as CLOSED.
- **Delta scores**: the suite-standard throughput-weighted epoch score,
  Delta = (SuccessScore - FailureScore) * Allowed / (Allowed + Blocked),
  computed per entry point on end-to-end outcomes and for the combined mesh
  stream. Entry requests reaching a crashed entry node count as allowed
  failures (nothing admitted or blocked them).
- **Node health**: mean % healthy lifetime and average crashes per node.
- **Concurrency amplification**: max over nodes of peak/avg backlog
  (executing + queued). Note this ratio penalizes governors that keep
  average backlog near zero; read it together with the dwell and health
  columns.

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
Candidate       |   Allowed |   Blocked |   Success |  Failures |  MeshDelta | Closed% |  Throt% |   Open% | Healthy% | AvgCrash | ConcRatio
No-Gov          |    519805 |         0 |    216186 |    303619 |    -7819.3 |  100.00 |    0.00 |    0.00 |    95.00 |     0.50 |      82.1
Levee           |    360233 |    159572 |    330511 |     29722 |     5848.1 |   84.99 |    6.86 |    8.16 |   100.00 |     0.00 |     112.2
Static-Nominal  |    408302 |    111503 |    107211 |    301091 |    -4431.3 |   73.70 |    4.46 |   21.84 |   100.00 |     0.00 |      65.9
Static-Peak     |    519805 |         0 |     95097 |    424708 |   -13368.2 |   76.22 |    2.17 |   21.61 |    98.00 |     0.20 |      82.1

Crashes: No-Gov: edge-api x2, db x2, payments x1. Static-Peak: edge-api x2.
         Levee and Static-Nominal: none.
```

How each candidate fails, and how Levee does not:

- **No-Gov** crashes edge-api twice in the surge, db twice under load and
  degradation, and payments in phase D. Every crash is a 120s outage plus a
  timeout storm at the entries.
- **Static-Peak** is sized to admit the surge, so the surge crashes edge-api
  exactly like No-Gov: a rate limiter tuned for the peak does not prevent the
  entry-node queue from saturating before autoscaling catches up. Its
  per-replica breakers then spend over a fifth of governor lifetime OPEN
  across the incident phases, converting a 15% error rate into 100% blocking
  on tripped edges.
- **Static-Nominal** never crashes anything -- by clamping the mesh to 1.5x
  steady state, forfeiting most of the surge. In phase C its breakers do
  trip on the degraded db, but in this scenario the binary breaker
  overcorrects: OPEN 22% of governor lifetime, oscillating between blocking
  healthy traffic and re-admitting failing traffic (failures ~301k despite
  zero crashes).
- **Levee** crashes nothing, admits 3.1x Static-Nominal's successful
  traffic, and posts the only positive MeshDelta at both entries. It rides
  the surge in THROTTLED while the autoscaler catches up, throttles into
  the degraded db in phase C, and keeps payments- and db-facing roles out
  of OPEN almost entirely. In steady phases it converges back to CLOSED.

Two mesh-mode behaviors worth knowing. First, because inbound Levee sees
subtree outcomes, a deep failure (payments down) pushes entry-node inbound
instances toward OPEN for part of the outage -- Levee defends the entry's
SLO by shedding at the door, trading some healthy browse traffic for
backpressure; the dwell table in the test output makes this visible per
role. Second, the async callback edge (notify -> orders) parks OPEN/
THROTTLED during stress, shedding optional work first -- callbacks never
affect their parent's outcome. Note that per-replica instances each adapt
on 1/R of the node's traffic, so high-replica nodes converge a little
slower than a single shared instance would; Levee still wins with that
handicap.

Known model biases, called out for fairness:

- The HPA sees only admitted traffic, so a governor that sheds hard also
  suppresses its own scale-up signal. This mirrors common autoscaler behavior,
  but it also compounds any governor's scale-up suppression.
- Static sizing constants (1.5x headroom, subtree-mean-latency inflight) are
  judgment calls; the derivations live in `mesh/static.go` so reviewers can
  dispute numbers rather than mechanism.
- Scale-down is effectively disabled (600s delay vs 20-minute sim); the
  scenario tests up-scaling lag only.

## Run it

From the `benchmarks/` directory:

```
# Full 5-phase scenario, ~2s wall per candidate
go test -v -run '^TestMeshBenchmark$' -timeout 10m

# Phases A+B only, for quick iteration
go test -v -run TestMeshBenchmarkShort -timeout 5m

# Engine and model unit tests
go test ./mesh/
```
