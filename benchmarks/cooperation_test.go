package benchmarks_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/levee/benchmarks/distributed/app"
	"github.com/codemartial/levee/benchmarks/distributed/backend"
	"github.com/codemartial/loadgen"
)

// coopInstance holds a single CB instance and its per-instance state.
type coopInstance struct {
	CB          app.CircuitBreaker
	Metrics     *app.CBMetrics
	Concurrency int64
	MaxConc     int64
}

// coopResult holds results for one CB type in the cooperation benchmark.
type coopResult struct {
	CBType       string
	Instances    int
	Metrics      api.CBMetrics // aggregated across instances
	BackendStats backend.ServerStats
	WallTime     time.Duration
}

// TestCooperationBenchmark tests multiple CB instances sharing a single backend.
//
// Architecture:
//
//	Dispatcher → round-robin → CB[0] ─┐
//	                           CB[1] ─┼→ Shared Backend
//	                           CB[2] ─┘
//
// Run with: go test -v -run TestCooperationBenchmark -timeout 60m
func TestCooperationBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping cooperation benchmark in short mode")
	}

	specs := benchmarks.GenerateCyberMondayWorkload()
	runCooperationBenchmark(t, specs, 100)
}

// TestCooperationBenchmarkFirstIncident runs the cooperation test for the first incident only.
//
// Run with: go test -v -run TestCooperationBenchmarkFirstIncident -timeout 15m
func TestCooperationBenchmarkFirstIncident(t *testing.T) {
	allSpecs := benchmarks.GenerateCyberMondayWorkload()
	specs := allSpecs[:8] // 4h calm + 1h midnight incident
	runCooperationBenchmark(t, specs, 100)
}


func runCooperationBenchmark(t *testing.T, specs []loadgen.LoadSpec, numInstances int) {
	t.Logf("Cooperation benchmark: %d instances sharing 1 backend, %.1f hours",
		numInstances, calculateTotalHours(specs))

	const seed uint64 = 20241225
	slo := levee.SLO{
		SuccessRate: 0.80,
		Timeout:     2500 * time.Millisecond,
	}
	capacityConfig := backend.CapacityControllerConfig{
		BaseShape: backend.BaseShape{
			ThroughputRPS: 150,
			QueueDepth:    50,
		},
		MinReplicas:            1,
		MaxReplicas:            8,
		TargetUtilization:      0.70,
		EvaluationInterval:     15 * time.Second,
		ScaleDownStabilization: 300 * time.Second,
		ScaleDownDelay:         600 * time.Second,
		ProvisioningLag:        30 * time.Second,
	}

	cbTypes := []string{"No-CB", "Levee", "Static-BAU", "Static-Peak"}
	var results []coopResult

	for _, cbType := range cbTypes {
		start := time.Now()
		metrics, bStats := runCoopBenchmark(cbType, numInstances, specs, slo, capacityConfig, seed)
		elapsed := time.Since(start)

		results = append(results, coopResult{
			CBType:       cbType,
			Instances:    numInstances,
			Metrics:      metrics,
			BackendStats: bStats,
			WallTime:     elapsed,
		})
		t.Logf("%s completed in %.1fs", cbType, elapsed.Seconds())
	}

	printCoopResults(t, results, numInstances, calculateTotalHours(specs))
}

// runCoopBenchmark runs N instances of a single CB type sharing one backend.
// Uses a sequential event loop with deterministic round-robin routing.
func runCoopBenchmark(
	cbType string,
	numInstances int,
	specs []loadgen.LoadSpec,
	slo levee.SLO,
	capacityConfig backend.CapacityControllerConfig,
	seed uint64,
) (api.CBMetrics, backend.ServerStats) {
	backendServer := backend.NewServer(specs, capacityConfig, seed)

	instances := make([]coopInstance, numInstances)
	for i := range instances {
		instances[i] = coopInstance{
			CB:      createCB(cbType, slo),
			Metrics: &app.CBMetrics{},
		}
	}

	// Shared epoch scorer — combines results from all instances before
	// applying squared scoring, so scores are comparable to isolated runs.
	sharedScorer := &app.CBMetrics{}

	onComplete := func(c api.Completion) {
		inst := &instances[c.Tag]
		completionTime := time.Unix(0, c.CompletionNS)
		if c.Success {
			inst.CB.Success(completionTime, c.Latency)
		} else {
			inst.CB.Fail(completionTime, c.Latency)
		}
		inst.Concurrency--
		inst.Metrics.RecordCompletion(c.Success, inst.Concurrency)
		sharedScorer.RecordResult(c.CompletionNS, c.Success)
	}

	// Sequential event loop with round-robin dispatch
	rng := rand.New(rand.NewPCG(seed, seed>>32))
	var logicalTimeNS int64
	var currentSpec int
	var specStartNS int64
	var requestCounter int64

	interArrivalTimeNS := (60.0 * 1e9) / float64(specs[0].RPM)

	for currentSpec < len(specs) {
		// Advance logical time (same RNG sequence as dispatcher)
		interval := rng.ExpFloat64() * interArrivalTimeNS
		logicalTimeNS += int64(interval)

		// Check spec transition — matches dispatcher's maybeAdvanceSpec:
		// updates spec but still processes the request (unless specs exhausted)
		specDurationNS := int64(specs[currentSpec].DurationS) * int64(time.Second)
		if logicalTimeNS-specStartNS >= specDurationNS {
			currentSpec++
			specStartNS = logicalTimeNS
			if currentSpec >= len(specs) {
				break
			}
			interArrivalTimeNS = (60.0 * 1e9) / float64(specs[currentSpec].RPM)
		}

		// Round-robin routing
		idx := int(requestCounter % int64(numInstances))
		requestCounter++

		// Advance backend time — delivers completions routed by Tag
		backendServer.Advance(logicalTimeNS, onComplete)

		ts := time.Unix(0, logicalTimeNS)
		inst := &instances[idx]

		sc, err := inst.CB.Start(ts)
		_ = sc

		if err != nil {
			inst.Metrics.RecordBlocked()
			continue
		}

		inst.Concurrency++
		inst.Metrics.RecordAllowed(inst.Concurrency, inst.CB.State())

		backendServer.Submit(api.BackendRequest{
			TimestampNS: logicalTimeNS,
			TimeoutMS:   int(specs[currentSpec].TimeoutMS),
			SpecIndex:   currentSpec,
			Tag:         idx,
		})
	}

	// Final drain
	backendServer.Advance(math.MaxInt64, onComplete)

	// Finalize and aggregate metrics
	return aggregateMetrics(instances, sharedScorer), backendServer.Stats()
}

// createCB creates a circuit breaker of the given type.
func createCB(cbType string, slo levee.SLO) app.CircuitBreaker {
	switch cbType {
	case "No-CB":
		return benchmarks.NewNoCB()
	case "Levee":
		return levee.NewLevee(slo)
	case "Static-BAU":
		return benchmarks.NewStaticBAU()
	case "Static-Peak":
		return benchmarks.NewStaticPeak()
	default:
		panic("unknown CB type: " + cbType)
	}
}

// aggregateMetrics combines per-instance counters and uses the shared epoch
// scorer for SuccessScore/FailureScore. This ensures epoch scoring is applied
// to the combined result stream, making scores comparable across instance counts.
func aggregateMetrics(instances []coopInstance, sharedScorer *app.CBMetrics) api.CBMetrics {
	var totalAllowed, totalBlocked, totalSuccesses, totalFailures int64
	var totalTransitions int
	var maxConc int64

	for _, inst := range instances {
		m := inst.Metrics.Snapshot()
		totalAllowed += m.TotalAllowed
		totalBlocked += m.TotalBlocked
		totalSuccesses += m.TotalSuccesses
		totalFailures += m.TotalFailures
		totalTransitions += m.StateTransitions
		if m.MaxConcurrency > maxConc {
			maxConc = m.MaxConcurrency
		}
	}

	sharedScorer.Finalize()
	scores := sharedScorer.Snapshot()

	return api.CBMetrics{
		TotalAllowed:     totalAllowed,
		TotalBlocked:     totalBlocked,
		TotalSuccesses:   totalSuccesses,
		TotalFailures:    totalFailures,
		StateTransitions: totalTransitions,
		SuccessScore:     scores.SuccessScore,
		FailureScore:     scores.FailureScore,
		MaxConcurrency:   maxConc,
	}
}

func printCoopResults(t *testing.T, results []coopResult, numInstances int, logicalHours float64) {
	t.Log("")
	t.Logf("Cooperation Benchmark Results (%d instances, %.0f hours simulated)", numInstances, logicalHours)
	t.Log("=" + repeatString("=", 120))
	t.Log("")

	// Header
	t.Logf("%-15s | %9s | %10s | %10s | %9s | %12s | %12s | %10s | %14s | %7s",
		"CB Type", "Instances", "Allowed", "Successes", "Failures",
		"SuccessScore", "FailureScore", "Delta", "MaxConcurrency", "Crashes")
	t.Log(repeatString("-", 16) + "+" + repeatString("-", 11) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 11) + "+" + repeatString("-", 14) + "+" +
		repeatString("-", 14) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 16) + "+" + repeatString("-", 9))

	for _, r := range results {
		rawDelta := r.Metrics.SuccessScore - r.Metrics.FailureScore
		allowedRatio := float64(r.Metrics.TotalAllowed) / float64(r.Metrics.TotalAllowed+r.Metrics.TotalBlocked)
		delta := rawDelta * allowedRatio

		t.Logf("%-15s | %9d | %10d | %10d | %9d | %12.2f | %12.2f | %10.2f | %14d | %7d",
			r.CBType,
			r.Instances,
			r.Metrics.TotalAllowed,
			r.Metrics.TotalSuccesses,
			r.Metrics.TotalFailures,
			r.Metrics.SuccessScore,
			r.Metrics.FailureScore,
			delta,
			r.Metrics.MaxConcurrency,
			r.BackendStats.TotalCrashes)
	}

	t.Log("")
	t.Log("Delta = (SuccessScore - FailureScore) * Allowed/(Allowed+Blocked)  [higher is better]")
	t.Log("")

	// Backend stats
	t.Log("Backend Processing Stats:")
	t.Logf("%-15s | %10s | %10s | %10s | %10s | %10s | %10s",
		"CB Type", "Requests", "Successes", "Failures", "Shed", "QueueDrops", "Crashes")
	t.Log(repeatString("-", 16) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12))
	for _, r := range results {
		bs := r.BackendStats
		t.Logf("%-15s | %10d | %10d | %10d | %10d | %10d | %10d",
			r.CBType, bs.TotalRequests, bs.TotalSuccesses, bs.TotalFailures,
			bs.TotalShed, bs.TotalQueueDrops, bs.TotalCrashes)
	}
	t.Log("")

	// Levee vs Static-Peak comparison
	var leveeDelta, peakDelta float64
	for _, r := range results {
		rawDelta := r.Metrics.SuccessScore - r.Metrics.FailureScore
		allowedRatio := float64(r.Metrics.TotalAllowed) / float64(r.Metrics.TotalAllowed+r.Metrics.TotalBlocked)
		delta := rawDelta * allowedRatio
		switch r.CBType {
		case "Levee":
			leveeDelta = delta
		case "Static-Peak":
			peakDelta = delta
		}
	}

	lead := leveeDelta - peakDelta
	label := ""
	if lead > 0 {
		label = " <-- Levee wins"
	}
	t.Logf("Levee vs Static-Peak: Levee=%.2f, Static-Peak=%.2f, Lead=%.2f%s",
		leveeDelta, peakDelta, lead, label)
}
