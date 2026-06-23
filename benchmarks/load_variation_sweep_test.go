package benchmarks_test

import (
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/levee/benchmarks"
	"github.com/codemartial/levee/benchmarks/distributed/backend"
	"github.com/codemartial/loadgen"
)

// TestLoadVariationSweep varies baseline RPM and error rate to verify Levee
// hasn't overfitted to the Cyber Monday workload parameters.
//
// Run with: go test -v -run TestLoadVariationSweep -timeout 120m
func TestLoadVariationSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load variation sweep in short mode")
	}

	baseSpecs := benchmarks.GenerateCyberMondayWorkload()

	type loadVariation struct {
		Name     string
		RPMScale float64 // multiplier on all RPM values
		ErrScale float64 // multiplier on all error rates (capped at 0.95)
	}

	variations := []loadVariation{
		{"baseline", 1.0, 1.0},
		{"half-RPM", 0.5, 1.0},
		{"double-RPM", 2.0, 1.0},
		{"low-err", 1.0, 0.2},
		{"high-err", 1.0, 3.0},
		{"low-RPM-high-err", 0.5, 3.0},
		{"high-RPM-low-err", 2.0, 0.2},
	}

	const seed uint64 = 20241225
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
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

	cbNames := []string{"Levee", "Static-BAU", "Static-Peak"}

	type sweepResult struct {
		Variation      string
		CB             string
		Allowed        int64
		Blocked        int64
		Successes      int64
		Failures       int64
		Delta          float64
		MaxConcurrency int64
	}

	var allResults []sweepResult

	for _, v := range variations {
		specs := scaleSpecs(baseSpecs, v.RPMScale, v.ErrScale)

		t.Logf("=== Running %s (RPM×%.1f, Err×%.1f) ===", v.Name, v.RPMScale, v.ErrScale)
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
		t.Logf("%s completed in %.1fs", v.Name, elapsed.Seconds())

		for r := range results {
			if r.Err != nil {
				t.Errorf("%s CB %s failed: %v", v.Name, r.Name, r.Err)
				continue
			}
			rawDelta := r.Metrics.SuccessScore - r.Metrics.FailureScore
			allowedRatio := float64(r.Metrics.TotalAllowed) / float64(r.Metrics.TotalAllowed+r.Metrics.TotalBlocked)
			delta := rawDelta * allowedRatio
			allResults = append(allResults, sweepResult{
				Variation:      v.Name,
				CB:             r.Name,
				Allowed:        r.Metrics.TotalAllowed,
				Blocked:        r.Metrics.TotalBlocked,
				Successes:      r.Metrics.TotalSuccesses,
				Failures:       r.Metrics.TotalFailures,
				Delta:          delta,
				MaxConcurrency: r.Metrics.MaxConcurrency,
			})
		}
	}

	// Print summary
	t.Log("")
	t.Log("Load Variation Sweep Summary")
	t.Log("============================")
	t.Log("")

	// Per-variation tables
	for _, v := range variations {
		t.Logf("%-25s (RPM×%.1f, Err×%.1f)", v.Name, v.RPMScale, v.ErrScale)
		t.Logf("%-15s | %10s | %9s | %12s | %14s",
			"CB", "Allowed", "Failures", "Delta", "MaxConcurrency")
		t.Logf("%-15s-+-%10s-+-%9s-+-%12s-+-%14s",
			"---------------", "----------", "---------", "------------", "--------------")
		for _, cb := range cbNames {
			for _, r := range allResults {
				if r.Variation == v.Name && r.CB == cb {
					t.Logf("%-15s | %10d | %9d | %12.2f | %14d",
						r.CB, r.Allowed, r.Failures, r.Delta, r.MaxConcurrency)
				}
			}
		}
		t.Log("")
	}

	// Levee vs Static-Peak comparison
	t.Log("Levee vs Static-Peak Delta Comparison:")
	t.Logf("%-25s | %12s | %12s | %12s", "Variation", "Levee", "Static-Peak", "Levee Lead")
	t.Logf("%-25s-+-%12s-+-%12s-+-%12s", "-------------------------", "------------", "------------", "------------")

	wins, losses := 0, 0
	for _, v := range variations {
		var leveeDelta, peakDelta float64
		for _, r := range allResults {
			if r.Variation == v.Name {
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
			marker = " WIN"
			wins++
		} else {
			marker = " LOSS"
			losses++
		}
		t.Logf("%-25s | %12.2f | %12.2f | %12.2f%s", v.Name, leveeDelta, peakDelta, lead, marker)
	}

	t.Logf("")
	t.Logf("Levee wins %d/%d variations", wins, wins+losses)

	// Raw CSV
	t.Log("")
	t.Log("Raw data (CSV):")
	t.Log("Variation,CB,Allowed,Blocked,Successes,Failures,Delta,MaxConcurrency")
	for _, r := range allResults {
		t.Logf("%s,%s,%d,%d,%d,%d,%.2f,%d",
			r.Variation, r.CB, r.Allowed, r.Blocked, r.Successes, r.Failures, r.Delta, r.MaxConcurrency)
	}
}

// scaleSpecs creates a copy of specs with RPM and error rates scaled.
func scaleSpecs(specs []loadgen.LoadSpec, rpmScale, errScale float64) []loadgen.LoadSpec {
	scaled := make([]loadgen.LoadSpec, len(specs))
	for i, s := range specs {
		scaled[i] = s
		scaled[i].RPM = max(int(float64(s.RPM)*rpmScale), 1)
		scaled[i].ErrorRate = min(s.ErrorRate*errScale, 0.95)
	}
	return scaled
}
