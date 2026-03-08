package benchmarks_test

import (
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/backend"
)

func TestQueueDepthSweep(t *testing.T) {
	specs := benchmarks.GenerateCyberMondayWorkload()

	// Queue depth multipliers: 0.2x, 0.5x, 1x, 1.4x, 2x of baseline (50)
	baseQueueDepth := 50
	multipliers := []float64{0.2, 0.5, 1.0, 1.4, 2.0}
	cbNames := []string{"Levee", "Static-BAU", "Static-Peak"}

	const seed uint64 = 20241225
	slo := levee.SLO{
		SuccessRate: 0.9,
		Timeout:     1500 * time.Millisecond,
	}

	type sweepResult struct {
		QueueDepthMult float64
		QueueDepth     int
		CB             string
		Failures       int64
		Delta          float64
		MaxConcurrency int64
		Crashes        int64
	}

	var allResults []sweepResult

	for _, mult := range multipliers {
		qd := int(float64(baseQueueDepth) * mult)
		if qd < 1 {
			qd = 1
		}

		capacityConfig := backend.CapacityControllerConfig{
			BaseShape: backend.BaseShape{
				ThroughputRPS: 150,
				QueueDepth:    qd,
			},
			MinReplicas:            1,
			MaxReplicas:            8,
			TargetUtilization:      0.70,
			EvaluationInterval:     15 * time.Second,
			ScaleDownStabilization: 300 * time.Second,
			ScaleDownDelay:         600 * time.Second,
			ProvisioningLag:        30 * time.Second,
		}

		t.Logf("=== Running QueueDepth=%d (%.1fx) ===", qd, mult)
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
		t.Logf("QueueDepth=%d completed in %.1fs", qd, elapsed.Seconds())

		for r := range results {
			if r.Err != nil {
				t.Errorf("QueueDepth=%d CB %s failed: %v", qd, r.Name, r.Err)
				continue
			}
			rawDelta := r.Metrics.SuccessScore - r.Metrics.FailureScore
			allowedRatio := float64(r.Metrics.TotalAllowed) / float64(r.Metrics.TotalAllowed+r.Metrics.TotalBlocked)
			delta := rawDelta * allowedRatio
			allResults = append(allResults, sweepResult{
				QueueDepthMult: mult,
				QueueDepth:     qd,
				CB:             r.Name,
				Failures:       r.Metrics.TotalFailures,
				Delta:          delta,
				MaxConcurrency: r.Metrics.MaxConcurrency,
				Crashes:        r.BackendStats.TotalCrashes,
			})
		}
	}

	// Print summary table
	t.Log("")
	t.Log("Queue Depth Sweep Summary")
	t.Log("=========================")
	t.Log("")

	cbOrder := []string{"Levee", "Static-BAU", "Static-Peak"}

	// Print per-queue-depth tables
	for _, mult := range multipliers {
		qd := int(float64(baseQueueDepth) * mult)
		if qd < 1 {
			qd = 1
		}
		t.Logf("QueueDepth = %d (%.1fx)", qd, mult)
		t.Logf("%-15s | %10s | %12s | %14s | %8s", "CB", "Failures", "Delta", "MaxConcurrency", "Crashes")
		t.Logf("%-15s-+-%10s-+-%12s-+-%14s-+-%8s", "---------------", "----------", "------------", "--------------", "--------")
		for _, cb := range cbOrder {
			for _, r := range allResults {
				if r.QueueDepthMult == mult && r.CB == cb {
					t.Logf("%-15s | %10d | %12.2f | %14d | %8d", r.CB, r.Failures, r.Delta, r.MaxConcurrency, r.Crashes)
				}
			}
		}
		t.Log("")
	}

	// Print Levee vs Static-Peak delta comparison
	t.Log("Levee vs Static-Peak Delta Comparison:")
	t.Logf("%-12s | %12s | %12s | %12s", "QueueDepth", "Levee", "Static-Peak", "Levee Lead")
	t.Logf("%-12s-+-%12s-+-%12s-+-%12s", "------------", "------------", "------------", "------------")
	for _, mult := range multipliers {
		qd := int(float64(baseQueueDepth) * mult)
		if qd < 1 {
			qd = 1
		}
		var leveeDelta, peakDelta float64
		for _, r := range allResults {
			if r.QueueDepthMult == mult {
				switch r.CB {
				case "Levee":
					leveeDelta = r.Delta
				case "Static-Peak":
					peakDelta = r.Delta
				}
			}
		}
		lead := leveeDelta - peakDelta
		label := ""
		if lead > 0 {
			label = " <-- Levee wins"
		}
		t.Logf("%4d (%.1fx)  | %12.2f | %12.2f | %12.2f%s", qd, mult, leveeDelta, peakDelta, lead, label)
	}

	// Print raw data for easy extraction
	t.Log("")
	t.Log("Raw data (CSV):")
	t.Log("QueueDepthMult,QueueDepth,CB,Failures,Delta,MaxConcurrency,Crashes")
	for _, r := range allResults {
		t.Logf("%.1f,%d,%s,%d,%.2f,%d,%d", r.QueueDepthMult, r.QueueDepth, r.CB, r.Failures, r.Delta, r.MaxConcurrency, r.Crashes)
	}
}
