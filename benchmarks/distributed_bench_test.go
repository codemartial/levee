package benchmarks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/distributed/api"
	"github.com/codemartial/levee/distributed/app"
	"github.com/codemartial/levee/distributed/backend"
	distloadgen "github.com/codemartial/levee/distributed/loadgen"
	"github.com/codemartial/loadgen"
)

// TestDistributedBenchmark runs the full distributed benchmark in-process using Unix sockets.
// This test simulates 28 hours of Cyber Monday traffic.
//
// Architecture (each CB runs in isolation):
//
//	[Load Generator] --unix--> [App Server] --unix--> [Backend Server]
//	                           (1 CB)                 (dedicated capacity)
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

// cbResult holds the result from running a single CB benchmark.
type cbResult struct {
	Name     string
	Metrics  api.CBMetrics
	WallTime time.Duration
	Err      error
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
			QueueDepth:    15, // 10% of throughput
		},
		MinReplicas:            1,
		MaxReplicas:            8,
		TargetUtilization:      0.70,
		EvaluationInterval:     15 * time.Second,
		ScaleDownStabilization: 300 * time.Second,
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
			metrics, wallTime, err := runSingleCB(t, name, specs, slo, capacityConfig, seed)
			results <- cbResult{
				Name:     name,
				Metrics:  metrics,
				WallTime: wallTime,
				Err:      err,
			}
		}(cbName)
	}

	wg.Wait()
	close(results)

	overallElapsed := time.Since(overallStart)

	// Collect results
	allMetrics := make(map[string]api.CBMetrics)
	var maxWallTime time.Duration
	var totalRequests int64

	for result := range results {
		if result.Err != nil {
			t.Errorf("CB %s failed: %v", result.Name, result.Err)
			continue
		}
		allMetrics[result.Name] = result.Metrics
		totalRequests += result.Metrics.TotalAllowed + result.Metrics.TotalBlocked
		if result.WallTime > maxWallTime {
			maxWallTime = result.WallTime
		}
		t.Logf("%s completed in %.1f seconds", result.Name, result.WallTime.Seconds())
	}

	// Print combined results
	printResults(t, allMetrics, totalRequests, calculateTotalHours(specs), overallElapsed)
}

// runSingleCB runs a single CB benchmark in isolation.
func runSingleCB(t *testing.T, cbName string, specs []loadgen.LoadSpec, slo levee.SLO, capacityConfig backend.CapacityControllerConfig, seed uint64) (api.CBMetrics, time.Duration, error) {
	// Create temp directory for this CB's Unix sockets
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("levee-bench-%s-*", cbName))
	if err != nil {
		return api.CBMetrics{}, 0, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	backendSocket := filepath.Join(tmpDir, "backend.sock")
	appSocket := filepath.Join(tmpDir, "app.sock")

	// Start backend server (dedicated for this CB)
	backendServer := backend.NewServer(specs, capacityConfig, seed)
	go func() {
		if err := backendServer.Run(backendSocket); err != nil {
			t.Logf("[%s] Backend server error: %v", cbName, err)
		}
	}()

	// Wait for backend socket to be ready
	if err := waitForSocket(backendSocket, 5*time.Second); err != nil {
		return api.CBMetrics{}, 0, fmt.Errorf("backend socket not ready: %w", err)
	}

	// Start app server with only this CB
	appServer := app.NewServer(app.ServerConfig{
		BackendHost: backendSocket,
		SLO:         slo,
		Specs:       specs,
		CBName:      cbName, // Run only this CB
	})
	go func() {
		if err := appServer.Run(appSocket); err != nil {
			t.Logf("[%s] App server error: %v", cbName, err)
		}
	}()

	// Wait for app socket to be ready
	if err := waitForSocket(appSocket, 5*time.Second); err != nil {
		return api.CBMetrics{}, 0, fmt.Errorf("app socket not ready: %w", err)
	}

	// Create and run the load generator
	dispatcher := distloadgen.NewDispatcher(distloadgen.DispatcherConfig{
		AppHost: appSocket,
		Specs:   specs,
		Seed:    seed,
	})

	startTime := time.Now()

	ctx := context.Background()
	if err := dispatcher.Run(ctx); err != nil && err != context.Canceled {
		return api.CBMetrics{}, 0, fmt.Errorf("dispatcher error: %w", err)
	}

	elapsed := time.Since(startTime)

	// Fetch metrics from app
	metrics, err := fetchMetrics(appSocket)
	if err != nil {
		return api.CBMetrics{}, elapsed, fmt.Errorf("failed to fetch metrics: %w", err)
	}

	// Extract this CB's metrics
	cbMetrics, ok := metrics.CircuitBreakers[cbName]
	if !ok {
		return api.CBMetrics{}, elapsed, fmt.Errorf("metrics not found for %s", cbName)
	}

	return cbMetrics, elapsed, nil
}

// waitForSocket waits for a Unix socket to become available.
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("socket %s not ready after %v", socketPath, timeout)
}

// fetchMetrics fetches metrics from the app server via Unix socket.
func fetchMetrics(socketPath string) (*api.AppMetrics, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get("http://unix/metrics")
	if err != nil {
		return nil, fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()

	var metrics api.AppMetrics
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	return &metrics, nil
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
func printResults(t *testing.T, cbMetrics map[string]api.CBMetrics, totalRequests int64, logicalHours float64, wallTime time.Duration) {
	t.Log("")
	t.Logf("Isolated Benchmark Results (%.0f hours simulated in %.1fs wall time)",
		logicalHours, wallTime.Seconds())
	t.Log("=" + repeatString("=", 119))
	t.Log("")

	// Header
	t.Logf("%-15s | %10s | %10s | %10s | %9s | %12s | %12s | %10s",
		"Candidate", "Blocked", "Allowed", "Successes", "Failures",
		"SuccessScore", "FailureScore", "Delta")
	t.Log(repeatString("-", 16) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 12) + "+" + repeatString("-", 12) + "+" +
		repeatString("-", 11) + "+" + repeatString("-", 14) + "+" +
		repeatString("-", 14) + "+" + repeatString("-", 11))

	// Data rows - ordered: No-CB, Levee, Static-BAU, Static-Peak
	cbOrder := []string{"No-CB", "Levee", "Static-BAU", "Static-Peak"}
	for _, name := range cbOrder {
		cb, ok := cbMetrics[name]
		if !ok {
			continue
		}

		// Delta = SuccessScore - FailureScore
		delta := cb.SuccessScore - cb.FailureScore

		t.Logf("%-15s | %10d | %10d | %10d | %9d | %12.2f | %12.2f | %10.2f",
			name,
			cb.TotalBlocked,
			cb.TotalAllowed,
			cb.TotalSuccesses,
			cb.TotalFailures,
			cb.SuccessScore,
			cb.FailureScore,
			delta)
	}

	t.Log("")
	t.Log("Delta = SuccessScore - FailureScore  [higher is better]")
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
