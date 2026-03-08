package benchmarks_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/levee/benchmarks/distributed/app"
	"github.com/codemartial/levee/benchmarks/distributed/backend"
	distloadgen "github.com/codemartial/levee/benchmarks/distributed/loadgen"
	"github.com/codemartial/loadgen"
)

// TestDistributedBenchmark runs the full distributed benchmark in-process.
// This test simulates 28 hours of Cyber Monday traffic.
//
// Architecture (each CB runs in isolation):
//
//	[Dispatcher] --> [Orchestrator] --> [Backend]
//	                 (1 CB)            (dedicated capacity)
//
// Run with: go test -v ./benchmarks -run TestDistributedBenchmark -timeout 120m
//
// The full simulation generates ~19M requests per CB and takes approximately 45 minutes.
// For a quicker test (first 4 hours only), use TestDistributedBenchmarkShort.
func TestDistributedBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping full benchmark in short mode; use TestDistributedBenchmarkShort instead")
	}

	specs := benchmarks.GenerateCyberMondayWorkload()
	runDistributedBenchmark(t, specs)
}

// TestDistributedBenchmarkShort runs a shortened version of the benchmark.
// Only simulates the first 4 hours of Cyber Monday (calm before the storm).
// Completes in approximately 2-3 minutes.
//
// Run with: go test -v ./benchmarks -run TestDistributedBenchmarkShort -timeout 10m
func TestDistributedBenchmarkShort(t *testing.T) {
	// Take only the first spec (4 hours of baseline traffic)
	allSpecs := benchmarks.GenerateCyberMondayWorkload()
	specs := allSpecs[:1] // First 4 hours only
	runDistributedBenchmark(t, specs)
}

// TestDistributedBenchmarkFirstIncident runs through the first incident.
// Simulates 5 hours: 4 hours of calm + the midnight spike and recovery.
// Completes in approximately 5-7 minutes.
//
// Run with: go test -v ./benchmarks -run TestDistributedBenchmarkFirstIncident -timeout 15m
func TestDistributedBenchmarkFirstIncident(t *testing.T) {
	allSpecs := benchmarks.GenerateCyberMondayWorkload()
	specs := allSpecs[:8] // 4h calm + 1h midnight incident
	runDistributedBenchmark(t, specs)
}

// cbResult holds the result from running a single CB benchmark.
type cbResult struct {
	Name         string
	Metrics      api.CBMetrics
	BackendStats backend.ServerStats
	WallTime     time.Duration
	Err          error
}

// runDistributedBenchmark runs each CB in isolation with its own backend.
func runDistributedBenchmark(t *testing.T, specs []loadgen.LoadSpec) {
	t.Logf("Running with %d load specs, total duration: %.1f hours",
		len(specs), calculateTotalHours(specs))
	t.Log("Each CB runs in isolation with its own dedicated backend")

	// Configuration
	const seed uint64 = 20241225
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
	}
	capacityConfig := backend.CapacityControllerConfig{
		BaseShape: backend.BaseShape{
			ThroughputRPS: 150,
			QueueDepth:    50, // absorb burst arrivals from exponential inter-arrival
		},
		MinReplicas:            1,
		MaxReplicas:            8,
		TargetUtilization:      0.70,
		EvaluationInterval:     15 * time.Second,
		ScaleDownStabilization: 300 * time.Second,
		ScaleDownDelay:         600 * time.Second,
		ProvisioningLag:        30 * time.Second,
	}

	cbNames := []string{"No-CB", "Levee", "Static-BAU", "Static-Peak"}

	// Run each CB in parallel with its own isolated backend
	results := make(chan cbResult, len(cbNames))
	var wg sync.WaitGroup

	overallStart := time.Now()

	for _, cbName := range cbNames {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			metrics, bStats, wallTime, err := runSingleCB(t, name, specs, slo, capacityConfig, seed)
			results <- cbResult{
				Name:         name,
				Metrics:      metrics,
				BackendStats: bStats,
				WallTime:     wallTime,
				Err:          err,
			}
		}(cbName)
	}

	wg.Wait()
	close(results)

	overallElapsed := time.Since(overallStart)

	// Collect results
	allMetrics := make(map[string]api.CBMetrics)
	allBackendStats := make(map[string]backend.ServerStats)
	var maxWallTime time.Duration
	var totalRequests int64

	for result := range results {
		if result.Err != nil {
			t.Errorf("CB %s failed: %v", result.Name, result.Err)
			continue
		}
		allMetrics[result.Name] = result.Metrics
		allBackendStats[result.Name] = result.BackendStats
		totalRequests += result.Metrics.TotalAllowed + result.Metrics.TotalBlocked
		if result.WallTime > maxWallTime {
			maxWallTime = result.WallTime
		}
		t.Logf("%s completed in %.1f seconds", result.Name, result.WallTime.Seconds())
	}

	// Print combined results
	printResults(t, allMetrics, allBackendStats, totalRequests, calculateTotalHours(specs), overallElapsed)
}

// runSingleCB runs a single CB benchmark in isolation.
func runSingleCB(t *testing.T, cbName string, specs []loadgen.LoadSpec, slo levee.SLO, capacityConfig backend.CapacityControllerConfig, seed uint64) (api.CBMetrics, backend.ServerStats, time.Duration, error) {
	// Create backend (dedicated for this CB)
	backendServer := backend.NewServer(specs, capacityConfig, seed)

	// Create orchestrator with backend advance/submit calls
	orchestrator := app.NewOrchestrator(app.OrchestratorConfig{
		Advance:   backendServer.Advance,
		Submit:    backendServer.Submit,
		SLO:       slo,
		Specs:     specs,
		StartTime: time.Unix(0, 0),
		CBName:    cbName,
	})

	// Create dispatcher with direct orchestrator call
	dispatcher := distloadgen.NewDispatcher(distloadgen.DispatcherConfig{
		HandleRequest: orchestrator.HandleRequest,
		Specs:         specs,
		Seed:          seed,
	})

	startTime := time.Now()

	ctx := context.Background()
	if err := dispatcher.Run(ctx); err != nil && err != context.Canceled {
		return api.CBMetrics{}, backend.ServerStats{}, 0, err
	}

	elapsed := time.Since(startTime)

	// Get metrics directly
	metrics := orchestrator.GetMetrics()
	bStats := backendServer.Stats()

	cbMetrics, ok := metrics.CircuitBreakers[cbName]
	if !ok {
		return api.CBMetrics{}, bStats, elapsed, nil
	}

	return cbMetrics, bStats, elapsed, nil
}

// calculateTotalHours returns the total duration of all specs in hours.
func calculateTotalHours(specs []loadgen.LoadSpec) float64 {
	var totalSeconds int64
	for _, spec := range specs {
		totalSeconds += int64(spec.DurationS)
	}
	return float64(totalSeconds) / 3600.0
}

// printResults prints the formatted results table.
func printResults(t *testing.T, cbMetrics map[string]api.CBMetrics, backendStats map[string]backend.ServerStats, totalRequests int64, logicalHours float64, wallTime time.Duration) {
	t.Log("")
	t.Logf("Isolated Benchmark Results (%.0f hours simulated in %.1fs wall time)",
		logicalHours, wallTime.Seconds())
	t.Log("=" + repeatString("=", 136))
	t.Log("")

	// Header
	t.Logf("%-15s | %10s | %10s | %10s | %9s | %12s | %12s | %10s | %14s",
		"Candidate", "Blocked", "Allowed", "Successes", "Failures",
		"SuccessScore", "FailureScore", "Delta", "MaxConcurrency")
	t.Log(repeatString("-", 16) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 11) + "+" + repeatString("-", 14) + "+" +
		repeatString("-", 14) + "+" + repeatString("-", 11) + "+" +
		repeatString("-", 16))

	// Data rows - ordered: No-CB, Levee, Static-BAU, Static-Peak
	cbOrder := []string{"No-CB", "Levee", "Static-BAU", "Static-Peak"}
	for _, name := range cbOrder {
		cb, ok := cbMetrics[name]
		if !ok {
			continue
		}

		// Delta = (SuccessScore - FailureScore) * Allowed/(Allowed+Blocked)
		rawDelta := cb.SuccessScore - cb.FailureScore
		allowedRatio := float64(cb.TotalAllowed) / float64(cb.TotalAllowed+cb.TotalBlocked)
		delta := rawDelta * allowedRatio

		t.Logf("%-15s | %10d | %10d | %10d | %9d | %12.2f | %12.2f | %10.2f | %14d",
			name,
			cb.TotalBlocked,
			cb.TotalAllowed,
			cb.TotalSuccesses,
			cb.TotalFailures,
			cb.SuccessScore,
			cb.FailureScore,
			delta,
			cb.MaxConcurrency)
	}

	t.Log("")
	t.Log("Delta = (SuccessScore - FailureScore) * Allowed/(Allowed+Blocked)  [higher is better]")
	t.Log("")

	// Backend stats
	t.Log("Backend Processing Stats:")
	t.Logf("%-15s | %10s | %10s | %10s | %10s | %10s | %10s",
		"Candidate", "Requests", "Successes", "Failures", "Shed", "QueueDrops", "Crashes")
	t.Log(repeatString("-", 16) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12))
	for _, name := range cbOrder {
		bs, ok := backendStats[name]
		if !ok {
			continue
		}
		t.Logf("%-15s | %10d | %10d | %10d | %10d | %10d | %10d",
			name, bs.TotalRequests, bs.TotalSuccesses, bs.TotalFailures, bs.TotalShed, bs.TotalQueueDrops, bs.TotalCrashes)
	}
	t.Log("")
	t.Logf("Total requests across all CBs: %d", totalRequests)
}

// repeatString repeats a string n times.
func repeatString(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
