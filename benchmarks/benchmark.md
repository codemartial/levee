# Goal

To find out whether Levee, with its self-tuning capability, is a superior solution to simpler, statically configured circuit breakers

# Approach

We compare circuit breakers against a **Prescient Breaker** - an oracle that knows the future from load specs and makes perfect open/close decisions with zero lag. This gives us ground truth for what the ideal behavior should be.

## Components

1. **Levee** - Self-tuning circuit breaker under test
2. **Static-BAU** - Statically configured CB tuned for normal business-as-usual traffic
3. **Static-Peak** - Statically configured CB tuned for peak traffic conditions
4. **Prescient** - Oracle CB that returns OPEN when load spec error rate exceeds SLO threshold
5. **loadgen** - Synthetic load event generator (`github.com/codemartial/loadgen`) that produces deterministic request streams independent of clocks and networks

## How It Works

The benchmark replays the same synthetic workload against all circuit breakers simultaneously. For each request, we know:
- The Prescient state (should traffic be blocked?)
- Each CB's decision (did it block or allow?)
- The request outcome (success or error from load spec)

This lets us compute exactly how each CB deviates from ideal behavior.

## Metrics

### State Transition Classifications

Each CB state change is classified relative to Prescient:

| Classification | Meaning |
|----------------|---------|
| **Good detection** | CB opened within 1s of Prescient opening |
| **Late detection** | CB opened >1s after Prescient opened |
| **Good recovery** | CB closed within 5s of Prescient closing |
| **Slow recovery** | CB closed >5s after Prescient closed |
| **False alarm** | CB opened when Prescient stayed closed (±5s window) |
| **Premature recovery** | CB closed while Prescient still open |
| **Flapping** | CB opened <1min after previous close |

### Penalty Scores

Raw request counts don't capture business impact. A request during peak traffic is worth more than one at 3 AM. We use RPS²-weighted penalties computed on-the-fly:

**BadTraffic Penalty** = √Σ(RPS²) for each second where Prescient is OPEN but CB allowed requests
- Measures damage from letting bad traffic through during incidents
- Higher RPS bleed-through is penalised more than lower RPS, since high load increases chances of catastrophe
- Lower is better

**LostBusiness Penalty** = √Σ(RPS²) for each second where Prescient is CLOSED but CB blocked requests
- Measures damage from blocking good traffic unnecessarily
- Higher RPS traffic is considered more valuable to business than lower RPS traffic
- Lower is better

The RPS² weighting means each blocked/allowed request is valued proportionally to the prevailing traffic rate. Blocking 1000 requests at 1000 RPS has 10x the penalty of blocking 1000 requests at 100 RPS.

**TotalPenalty** = √BadTraffic + √LostBusiness (displayed as root-sum for readability)


## The Workload

loadgen allows us to specify a workload as a sequence of load profile specs. These can be programmatically generated to allow for differently timed specs depending on the situation. Long periods of stable operation can be a spec with a long duration. Periods of rising instability can be more fine grained (5-minute intervals or 1-minute intervals, up to us). The benchmark's load story is as follows:

### The Story -- A day running Cyber Monday Sales

The company has set up hourly sale events starting 00:00 all the way through to 23:00. Our Levee and its opponent CB are both protecting a downstream that sees traffic spikes at the top of each hour. Our benchmark starts 4 hours before Cyber Monday. Things are calm and chugging along. Then at the stroke of midnight, the traffic jumps to 15x and the backend falls over within seconds because it was under provisioned. Retries make it worse, pushing traffic to 20x with 60% errors. The SREs enable auto-scaling for the next event. For subsequent hourly spikes, we see brief degradation (6-10% errors) but quick recovery as autoscaler capacity comes online. The SREs increase the minimum instance count and the sale events run fine until 10 AM with the spikes getting bigger as the morning goes.

At 10 AM, we see the biggest traffic spike - 8x of BAU which is now at 6x, totaling ~48x of baseline! The initial surge hits 25% errors, then retries push it to 35% before recovery. A few other things are learned over time:

1. The BAU traffic (between hourly events) has been creeping up and has reached 6x by 10 AM.
2. The hourly spikes are 3-5x of the instantaneous BAU traffic during morning hours (hours 1-9), and 3-4x during afternoon/evening.
3. Each spike reliably decays within 5 minutes. The pattern is: initial spike → (sometimes) worse retry storm → gradual recovery.
4. The BAU traffic peaks at 8x around mid-day and gradually dies down to ~2x by midnight.
5. There is another ~6x spike at 6 PM (totaling ~48x of baseline), following the same initial-then-worse pattern.
6. As the day wears on and more transactions are booked, the DB starts slowing down causing both latency AND error rates to creep up:
   - Hours 10-18: degradation creeps from 1x to 1.5x
   - Hours 18-24: degradation creeps from 1.5x to 3x

# Running the Benchmark

```bash
go test -bench=BenchmarkCyberMondayPrescient -benchtime=1x -run=^$ -v
```

This generates:
- `transition_log_raw.txt` - Every state transition from all CBs
- `transition_log_classified.txt` - Classified transitions with incident summaries
- Console summary comparing all CBs

# Evaluation

The benchmark produces a comparative summary:

```
Candidate       |    Blocked |    Allowed | Flap | FalseAlarm | LateDetect |   BadTraffic | LostBusiness | TotalPenalty
----------------------------------------------------------------------------------------------------------------------------------
Prescient       |     773832 |   55751328 |    0 |          0 |          0 |            0 |            0 |            0
Levee           |    3989544 |   52535616 |    1 |          3 |          0 |         1679 |         4435 |         6114
Static-Peak     |    3737675 |   52787485 |    4 |          0 |          1 |        24363 |         5623 |        29986
Static-BAU      |    4249310 |   52275850 |   35 |          8 |          0 |        13032 |        29844 |        42875
```

## The Candidates

**Prescient**: This is the reference circuit breaker that has foreknowledge of the traffic and errors so it opens and closes exactly where needed
**Levee**: The self-configuring circuit breaker. 
**Static-BAU**: A static circuit breaker that is most optimally tuned for the BAU average workload
**Static-Peak**: The same static circuit breaker but most optimally tuned for high traffic

## How to Read the Results

**Blocked/Allowed**: Raw request counts. More blocking isn't necessarily better - it depends on *when* you block.

**Flap**: Number of times CB opened within 1 minute of closing. High flapping indicates instability.

**FalseAlarm**: CB opened when backend was healthy. Causes unnecessary request rejection.

**LateDetect**: CB was slow to open after backend became unhealthy. Lets bad traffic through.

**BadTraffic** (√Σ RPS²): Damage from letting requests through during incidents. See [Penalty Scores](#penalty-scores) for details.

**LostBusiness** (√Σ RPS²): Damage from blocking requests when backend is healthy. See [Penalty Scores](#penalty-scores) for details.

**TotalPenalty**: Combined score between BadTraffic and LostBusiness. Lower is better.

## Ranking

The results illustrate the **sensitivity vs. specificity trade-off** that static configurations fail at.

- **Levee**: Low flapping and false alarms. No late detection.
  Lowest BadTraffic (13x lower than Static-Peak, 7x lower than Static-BAU).
  Lowest TotalPenalty at 6,114 — almost 5x better than Static-Peak, ~ 7x better than Static-BAU.
- **Static-Peak**: Conservative (low LostBusiness, minimal flapping)
  but slow to detect (high BadTraffic at ~ 24k)
- **Static-BAU**: The most likely configuration is also the worst
  performer with highest flapping, false alarms, and TotalPenalty

## Limitations

This benchmark is *open-loop*: circuit breaker actions do not influence the state of the upstream caller or the downstream dependency. This specifically fails to capture the benefits of Levee's concurrency limiting. The [distributed benchmark](distributed_benchmark.md) addresses this with a closed-loop simulation where CB decisions affect backend load, autoscaling, and crash behaviour — revealing that Levee's concurrency control (1,913 max concurrent vs 7,000+ for static CBs) prevents backend crashes that this benchmark cannot capture.

As with all things automated, explainability and predictability are
inversely proportional to the decision-making capacity of the
agent. You are advised to use simulation testing to determine
real-world suitability, as well as focus on relevant real-world
outcomes (e.g. aggregate *BadTraffic* and *LostBusiness* scores in this
benchmark) rather than stressing on specific behavioural incidents.
