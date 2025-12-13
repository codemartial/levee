package levee

import (
	"github.com/codemartial/loadgen"
)

// generateCyberMondayWorkload creates the complete 28-hour Cyber Monday story
// Starting 4 hours before midnight (8 PM on Sunday) through 24 hours of Cyber Monday
func generateCyberMondayWorkload() []loadgen.LoadSpec {
	specs := make([]loadgen.LoadSpec, 0, 100)

	// Fixed parameters from benchmark spec
	const (
		baselineRPM      = 6000  // 100 RPS baseline (1x traffic)
		healthyErrorRate = 0.005 // 0.5% healthy error rate
		nominalP50       = 50.0  // 50ms p50
		nominalP99       = 150.0 // 150ms p99
		timeout          = 1500.0
	)

	// Phase 1: Sunday evening (8 PM - Midnight) - 4 hours of calm before the storm
	// Steady 1x traffic, everything is peaceful
	specs = append(specs, loadgen.LoadSpec{
		RPM:          baselineRPM,
		ErrorRate:    healthyErrorRate,
		DurationS:    4 * 3600, // 4 hours
		P50LatencyMS: nominalP50,
		P99LatencyMS: nominalP99,
		TimeoutMS:    timeout,
	})

	// Phase 2: Midnight spike (Hour 0) - 20x spike, backend falls over immediately
	// Under-provisioned backend dies within seconds - CAUSE: traffic spike, EFFECT: errors + latency
	specs = append(specs, loadgen.LoadSpec{
		RPM:          20 * baselineRPM, // 20x spike (CAUSE)
		ErrorRate:    0.50,             // 50% errors - catastrophic failure (EFFECT)
		DurationS:    120,              // 2 minutes of peak chaos
		P50LatencyMS: 500.0,            // 10x slower (EFFECT of overload)
		P99LatencyMS: 1200.0,           // Near timeout
		TimeoutMS:    timeout,
	})

	// Immediate aftermath - traffic still high, backend dying
	specs = append(specs, loadgen.LoadSpec{
		RPM:          15 * baselineRPM, // Still 15x (some users retry)
		ErrorRate:    0.60,             // Even worse - retry storm
		DurationS:    180,              // 3 more minutes
		P50LatencyMS: 800.0,            // Getting worse
		P99LatencyMS: 1400.0,
		TimeoutMS:    timeout,
	})

	// SREs enable autoscaling - traffic starts backing off, backend starts recovering
	specs = append(specs, loadgen.LoadSpec{
		RPM:          8 * baselineRPM, // 8x (many gave up)
		ErrorRate:    0.15,            // Still elevated but improving as instances spin up
		DurationS:    180,             // 3 minutes
		P50LatencyMS: 300.0,           // Improving
		P99LatencyMS: 800.0,
		TimeoutMS:    timeout,
	})

	// More autoscaler capacity online
	specs = append(specs, loadgen.LoadSpec{
		RPM:          6 * baselineRPM, // 6x traffic (continuing to drop)
		ErrorRate:    0.05,            // Getting close to healthy
		DurationS:    240,             // 4 minutes
		P50LatencyMS: 150.0,
		P99LatencyMS: 400.0,
		TimeoutMS:    timeout,
	})

	// Backend nearly recovered - autoscaler has sufficient capacity
	specs = append(specs, loadgen.LoadSpec{
		RPM:          4 * baselineRPM, // 4x traffic
		ErrorRate:    0.02,            // Almost baseline
		DurationS:    300,             // 5 minutes
		P50LatencyMS: 80.0,
		P99LatencyMS: 250.0,
		TimeoutMS:    timeout,
	})

	// Fully recovered - back to elevated but healthy traffic
	specs = append(specs, loadgen.LoadSpec{
		RPM:          3 * baselineRPM, // 3x traffic
		ErrorRate:    0.008,           // Just slightly above baseline
		DurationS:    180,             // 3 minutes
		P50LatencyMS: 65.0,
		P99LatencyMS: 200.0,
		TimeoutMS:    timeout,
	})

	// Back to normal operations
	specs = append(specs, loadgen.LoadSpec{
		RPM:          2 * baselineRPM, // 2x BAU
		ErrorRate:    healthyErrorRate,
		DurationS:    2700, // Rest of hour 0 (45 minutes)
		P50LatencyMS: 60.0,
		P99LatencyMS: 180.0,
		TimeoutMS:    timeout,
	})

	// Hours 1-9: Rising morning traffic with autoscaling enabled
	// Each hour has a spike at the top of the hour (sales event)
	// BAU traffic gradually increases: 1x -> 6x by 10 AM
	// Autoscaler is now enabled, so spikes cause brief degradation but quick recovery
	for hour := 1; hour <= 9; hour++ {
		bauTraffic := 1.0 + (5.0 * float64(hour) / 10.0) // Linear increase from 1x to 6x
		bauRPM := bauTraffic * float64(baselineRPM)

		// Top of hour: Sales event spike - backend briefly overwhelmed
		// Spike is 3-5x of current BAU (not 10-15x, that was too much)
		spikeMultiplier := 3.0 + 2.0*(float64(hour)/10.0) // 3x-5x of BAU
		spikeRPM := bauTraffic * spikeMultiplier * float64(baselineRPM)

		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(spikeRPM),
			ErrorRate:    0.08, // 8% errors during spike
			DurationS:    60,   // 1 minute spike
			P50LatencyMS: nominalP50 * 2.5,
			P99LatencyMS: nominalP99 * 4.0,
			TimeoutMS:    timeout,
		})

		// Autoscaler catches up - errors drop, latency normalizes
		// 4 minute recovery as new instances come online
		for min := 1; min <= 4; min++ {
			decayFactor := 1.0 - (0.20 * float64(min)) // Faster decay
			currentRPM := bauRPM * (1.0 + (spikeMultiplier-1.0)*decayFactor)
			currentErrors := 0.08 * decayFactor // Errors drop as capacity increases
			if currentErrors < healthyErrorRate {
				currentErrors = healthyErrorRate
			}

			specs = append(specs, loadgen.LoadSpec{
				RPM:          int(currentRPM),
				ErrorRate:    currentErrors,
				DurationS:    60,
				P50LatencyMS: nominalP50 * (1 + 1.5*decayFactor),
				P99LatencyMS: nominalP99 * (1 + 3.0*decayFactor),
				TimeoutMS:    timeout,
			})
		}

		// Rest of hour: Stable BAU traffic at current level
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM),
			ErrorRate:    healthyErrorRate,
			DurationS:    55 * 60, // 55 minutes
			P50LatencyMS: nominalP50,
			P99LatencyMS: nominalP99,
			TimeoutMS:    timeout,
		})
	}

	// Hour 10: THE BIG ONE - Massive traffic spike
	// BAU is now 6x, spike is 8x of that = 48x original baseline
	const hour10BAU = 6.0
	const hour10BauRPM = hour10BAU * float64(baselineRPM)

	// The massive 10 AM spike - 8x current BAU
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 8.0), // 48x original
		ErrorRate:    0.30,                    // 30% errors - severe overload
		DurationS:    120,                     // 2 minutes
		P50LatencyMS: 600.0,
		P99LatencyMS: 1300.0,
		TimeoutMS:    timeout,
	})

	// Still very high traffic, autoscaler scrambling
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 6.0),
		ErrorRate:    0.20, // Improving
		DurationS:    180,  // 3 minutes
		P50LatencyMS: 400.0,
		P99LatencyMS: 1000.0,
		TimeoutMS:    timeout,
	})

	// Autoscaler catching up
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 4.0),
		ErrorRate:    0.10,
		DurationS:    300, // 5 minutes
		P50LatencyMS: 200.0,
		P99LatencyMS: 600.0,
		TimeoutMS:    timeout,
	})

	// Recovery to BAU with increased capacity
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 2.0),
		ErrorRate:    0.02,
		DurationS:    600, // 10 minutes
		P50LatencyMS: 80.0,
		P99LatencyMS: 250.0,
		TimeoutMS:    timeout,
	})

	// Rest of hour 10 - back to normal
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM),
		ErrorRate:    healthyErrorRate,
		DurationS:    44 * 60, // 44 minutes
		P50LatencyMS: nominalP50,
		P99LatencyMS: nominalP99,
		TimeoutMS:    timeout,
	})

	// Hours 11-17: Peak traffic holds at 6-8x with gradual latency creep
	// DB is slowing down as transactions accumulate (latency increases even at same load)
	for hour := 11; hour <= 17; hour++ {
		bauTraffic := 6.0 + 2.0*(float64(hour-11)/7.0)        // 6x -> 8x peak at midday
		latencyMultiplier := 1.0 + 2.0*(float64(hour-11)/7.0) // Latency creeps to 3x
		bauRPM := bauTraffic * float64(baselineRPM)

		// Hourly spike (4x of current BAU)
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM * 4.0),
			ErrorRate:    0.06, // 6% errors - mild degradation
			DurationS:    60,
			P50LatencyMS: nominalP50 * latencyMultiplier * 2.0,
			P99LatencyMS: nominalP99 * latencyMultiplier * 3.0,
			TimeoutMS:    timeout,
		})

		// Decay over 4 minutes
		for min := 1; min <= 4; min++ {
			decayFactor := 1.0 - (0.20 * float64(min))
			currentRPM := bauRPM * (1.0 + 3.0*decayFactor)
			currentErrors := 0.06 * decayFactor
			if currentErrors < healthyErrorRate {
				currentErrors = healthyErrorRate
			}

			specs = append(specs, loadgen.LoadSpec{
				RPM:          int(currentRPM),
				ErrorRate:    currentErrors,
				DurationS:    60,
				P50LatencyMS: nominalP50 * latencyMultiplier * (1 + decayFactor),
				P99LatencyMS: nominalP99 * latencyMultiplier * (1 + 2*decayFactor),
				TimeoutMS:    timeout,
			})
		}

		// BAU for rest of hour - latency creeping up due to DB slowdown
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM),
			ErrorRate:    healthyErrorRate,
			DurationS:    55 * 60,
			P50LatencyMS: nominalP50 * latencyMultiplier,
			P99LatencyMS: nominalP99 * latencyMultiplier,
			TimeoutMS:    timeout,
		})
	}

	// Hour 18 (6 PM): Second mega-spike with degraded DB
	const hour18BAU = 8.0
	const hour18BauRPM = hour18BAU * float64(baselineRPM)
	const hour18Latency = 3.0 // 3x latency due to DB slowdown

	// 6 PM spike - 6x current BAU
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 6.0), // 48x original
		ErrorRate:    0.25,                    // 25% errors - DB is slow
		DurationS:    120,                     // 2 minutes
		P50LatencyMS: nominalP50 * hour18Latency * 2.5,
		P99LatencyMS: nominalP99 * hour18Latency * 3.0,
		TimeoutMS:    timeout,
	})

	// Recovery - autoscaler + DB catching up
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 4.0),
		ErrorRate:    0.15,
		DurationS:    180, // 3 minutes
		P50LatencyMS: nominalP50 * hour18Latency * 2.0,
		P99LatencyMS: nominalP99 * hour18Latency * 2.5,
		TimeoutMS:    timeout,
	})

	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 2.0),
		ErrorRate:    0.05,
		DurationS:    600, // 10 minutes
		P50LatencyMS: nominalP50 * hour18Latency * 1.5,
		P99LatencyMS: nominalP99 * hour18Latency * 2.0,
		TimeoutMS:    timeout,
	})

	// Rest of hour 18
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM),
		ErrorRate:    healthyErrorRate,
		DurationS:    45 * 60, // 45 minutes
		P50LatencyMS: nominalP50 * hour18Latency,
		P99LatencyMS: nominalP99 * hour18Latency,
		TimeoutMS:    timeout,
	})

	// Hours 19-23: Wind down from 8x to 2x, latency still at 3x (DB still slow)
	for hour := 19; hour <= 23; hour++ {
		bauTraffic := 8.0 - (6.0 * float64(hour-19) / 5.0) // 8x -> 2x
		bauRPM := bauTraffic * float64(baselineRPM)

		// Hourly spike (3x of current BAU)
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM * 3.0),
			ErrorRate:    0.04, // 4% errors - DB still slow
			DurationS:    60,
			P50LatencyMS: nominalP50 * hour18Latency * 1.5,
			P99LatencyMS: nominalP99 * hour18Latency * 2.0,
			TimeoutMS:    timeout,
		})

		// Decay
		for min := 1; min <= 4; min++ {
			decayFactor := 1.0 - (0.20 * float64(min))
			currentRPM := bauRPM * (1.0 + 2.0*decayFactor)
			currentErrors := 0.04 * decayFactor
			if currentErrors < healthyErrorRate {
				currentErrors = healthyErrorRate
			}

			specs = append(specs, loadgen.LoadSpec{
				RPM:          int(currentRPM),
				ErrorRate:    currentErrors,
				DurationS:    60,
				P50LatencyMS: nominalP50 * hour18Latency * (1 + 0.5*decayFactor),
				P99LatencyMS: nominalP99 * hour18Latency * (1 + decayFactor),
				TimeoutMS:    timeout,
			})
		}

		// BAU
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM),
			ErrorRate:    healthyErrorRate,
			DurationS:    55 * 60,
			P50LatencyMS: nominalP50 * hour18Latency,
			P99LatencyMS: nominalP99 * hour18Latency,
			TimeoutMS:    timeout,
		})
	}

	return specs
}

func stateString(s State) string {
	switch s {
	case INIT:
		return "INIT"
	case CLOSED:
		return "CLOSED"
	case OPEN:
		return "OPEN"
	case HALF_OPEN:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}
