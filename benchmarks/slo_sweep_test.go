package benchmarks_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/levee/benchmarks/distributed/backend"
)

func TestSLOSweep(t *testing.T) {
	specs := benchmarks.GenerateCyberMondayWorkload()

	sloValues := []float64{0.99, 0.95, 0.9, 0.8, 0.7}
	cbNames := []string{"Levee", "Static-BAU", "Static-Peak"}

	const seed uint64 = 20241225
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

	type sweepResult struct {
		SLO            float64
		CB             string
		Failures       int64
		Delta          float64
		MaxConcurrency int64
	}

	var allResults []sweepResult

	for _, sloRate := range sloValues {
		slo := levee.SLO{
			SuccessRate: sloRate,
			Timeout:     1500 * time.Millisecond,
		}

		t.Logf("=== Running SLO %.1f ===", sloRate)
		start := time.Now()

		results := make(chan cbResult, len(cbNames))
		var wg sync.WaitGroup

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

		elapsed := time.Since(start)
		t.Logf("SLO %.1f completed in %.1fs", sloRate, elapsed.Seconds())

		for r := range results {
			if r.Err != nil {
				t.Errorf("SLO %.1f CB %s failed: %v", sloRate, r.Name, r.Err)
				continue
			}
			rawDelta := r.Metrics.SuccessScore - r.Metrics.FailureScore
			allowedRatio := float64(r.Metrics.TotalAllowed) / float64(r.Metrics.TotalAllowed+r.Metrics.TotalBlocked)
			delta := rawDelta * allowedRatio
			allResults = append(allResults, sweepResult{
				SLO:            sloRate,
				CB:             r.Name,
				Failures:       r.Metrics.TotalFailures,
				Delta:          delta,
				MaxConcurrency: r.Metrics.MaxConcurrency,
			})
		}
	}

	// Print summary table
	t.Log("")
	t.Log("SLO Sweep Summary")
	t.Log("=================")
	t.Log("")

	// Group by CB for comparison
	cbOrder := []string{"Levee", "Static-BAU", "Static-Peak"}

	// Print per-SLO tables
	for _, sloRate := range sloValues {
		t.Logf("SLO = %.1f", sloRate)
		t.Logf("%-15s | %10s | %12s | %14s", "CB", "Failures", "Delta", "MaxConcurrency")
		t.Logf("%-15s-+-%10s-+-%12s-+-%14s", "---------------", "----------", "------------", "--------------")
		for _, cb := range cbOrder {
			for _, r := range allResults {
				if r.SLO == sloRate && r.CB == cb {
					t.Logf("%-15s | %10d | %12.2f | %14d", r.CB, r.Failures, r.Delta, r.MaxConcurrency)
				}
			}
		}
		t.Log("")
	}

	// Print Levee vs Static-Peak delta comparison
	t.Log("Levee vs Static-Peak Delta Comparison:")
	t.Logf("%-6s | %12s | %12s | %12s", "SLO", "Levee", "Static-Peak", "Levee Lead")
	t.Logf("%-6s-+-%12s-+-%12s-+-%12s", "------", "------------", "------------", "------------")
	for _, sloRate := range sloValues {
		var leveeDelta, peakDelta float64
		for _, r := range allResults {
			if r.SLO == sloRate {
				switch r.CB {
				case "Levee":
					leveeDelta = r.Delta
				case "Static-Peak":
					peakDelta = r.Delta
				}
			}
		}
		lead := leveeDelta - peakDelta
		marker := ""
		if lead > 0 {
			marker = " <-- Levee wins"
		}
		t.Logf("%-6.2f | %12.2f | %12.2f | %12.2f%s", sloRate, leveeDelta, peakDelta, lead, marker)
	}

	// Print raw data for easy extraction
	t.Log("")
	t.Log("Raw data (CSV):")
	t.Log("SLO,CB,Failures,Delta,MaxConcurrency")
	for _, r := range allResults {
		t.Logf("%.1f,%s,%d,%.2f,%d", r.SLO, r.CB, r.Failures, r.Delta, r.MaxConcurrency)
	}

	// Verify Levee wins at SLO 0.9
	for _, r := range allResults {
		if r.SLO == 0.9 && r.CB == "Levee" {
			for _, p := range allResults {
				if p.SLO == 0.9 && p.CB == "Static-Peak" {
					if r.Delta <= p.Delta {
						t.Errorf("Levee (%.2f) should beat Static-Peak (%.2f) at SLO 0.9", r.Delta, p.Delta)
					}
				}
			}
		}
	}
}

// reuse types from distributed_bench_test.go (cbResult is defined there)
var _ = fmt.Sprintf // suppress unused import
var _ api.CBMetrics // suppress unused import
