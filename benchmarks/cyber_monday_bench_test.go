package benchmarks

import (
	"github.com/codemartial/levee"
	"github.com/codemartial/loadgen"
)

// bauDegradation returns the BAU degradation multiplier for a given hour.
// This affects both latency and error rates.
// Phase 1: 1x -> 1.5x between 10 AM spike (hour 10) and 6 PM spike (hour 18)
// Phase 2: 1.5x -> 3x after 6 PM spike (hours 18-23)
func bauDegradation(hour int) float64 {
	if hour < 10 {
		return 1.0
	}
	if hour < 18 {
		// Linear creep from 1.0 at hour 10 to 1.5 at hour 18
		return 1.0 + 0.5*float64(hour-10)/8.0
	}
	// Linear creep from 1.5 at hour 18 to 3.0 at hour 23
	return 1.5 + 1.5*float64(hour-18)/5.0
}

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

	// Phase 2: Midnight spike (Hour 0) - 15x spike, backend falls over immediately
	// Under-provisioned backend dies within seconds - CAUSE: traffic spike, EFFECT: errors + latency
	specs = append(specs, loadgen.LoadSpec{
		RPM:          15 * baselineRPM, // 15x spike (CAUSE)
		ErrorRate:    0.50,             // 50% errors - catastrophic failure (EFFECT)
		DurationS:    120,              // 2 minutes of peak chaos
		P50LatencyMS: 500.0,            // 10x slower (EFFECT of overload)
		P99LatencyMS: 1200.0,           // Near timeout
		TimeoutMS:    timeout,
	})

	// Immediate aftermath - retry storm makes it WORSE
	specs = append(specs, loadgen.LoadSpec{
		RPM:          20 * baselineRPM, // 20x - retry storm amplifies traffic
		ErrorRate:    0.60,             // Even worse - backend overwhelmed
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
		DurationS:    2400, // Rest of hour 0 (40 minutes) - total hour = 2+3+3+4+5+3+40 = 60 min
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
		// Spike is 3-5x of current BAU
		spikeMultiplier := 3.0 + 2.0*(float64(hour)/10.0) // 3x-5x of BAU
		spikeRPM := bauTraffic * spikeMultiplier * float64(baselineRPM)

		// Initial spike - bad but not worst yet
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(spikeRPM), // Full spike
			ErrorRate:    0.08,          // 8% errors
			DurationS:    120,           // 2 minutes
			P50LatencyMS: nominalP50 * 2.5,
			P99LatencyMS: nominalP99 * 4.0,
			TimeoutMS:    timeout,
		})

		// Autoscaler catches up - errors drop, latency normalizes
		// 3 minute recovery as new instances come online
		for min := 1; min <= 3; min++ {
			decayFactor := 1.0 - (0.25 * float64(min)) // Decay over 3 minutes
			currentRPM := bauRPM * (1.0 + (spikeMultiplier-1.0)*decayFactor)
			currentErrors := 0.06 * decayFactor // Errors drop as capacity increases
			currentErrors = max(currentErrors, healthyErrorRate)

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
			DurationS:    55 * 60, // 55 minutes - total hour = 2+3+55 = 60 min
			P50LatencyMS: nominalP50,
			P99LatencyMS: nominalP99,
			TimeoutMS:    timeout,
		})
	}

	// Hour 10: THE BIG ONE - Massive traffic spike
	// BAU is now 6x, spike is 8x of that = 48x original baseline
	const hour10BAU = 6.0
	const hour10BauRPM = hour10BAU * float64(baselineRPM)

	// Initial 10 AM spike - bad but not worst yet
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 6.0), // 36x original - initial surge
		ErrorRate:    0.25,                    // 25% errors
		DurationS:    120,                     // 2 minutes
		P50LatencyMS: 500.0,
		P99LatencyMS: 1100.0,
		TimeoutMS:    timeout,
	})

	// Retry storm - things get WORSE
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour10BauRPM * 8.0), // 48x original - retry storm peaks
		ErrorRate:    0.35,                    // 35% errors - worse
		DurationS:    180,                     // 3 minutes
		P50LatencyMS: 700.0,
		P99LatencyMS: 1350.0,
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
		DurationS:    40 * 60, // 40 minutes - total hour = 2+3+5+10+40 = 60 min
		P50LatencyMS: nominalP50,
		P99LatencyMS: nominalP99,
		TimeoutMS:    timeout,
	})

	// Hours 11-17: Peak traffic holds at 6-8x with gradual degradation
	// DB is slowing down as transactions accumulate (latency and errors increase)
	for hour := 11; hour <= 17; hour++ {
		bauTraffic := 6.0 + 2.0*(float64(hour-11)/7.0) // 6x -> 8x peak at midday
		degradation := bauDegradation(hour)
		bauRPM := bauTraffic * float64(baselineRPM)

		// Full Spike with a few retries
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM * 4.0), // Full spike with retries
			ErrorRate:    0.08,              // 8% errors - worse
			DurationS:    120,
			P50LatencyMS: nominalP50 * degradation * 2.0,
			P99LatencyMS: nominalP99 * degradation * 3.0,
			TimeoutMS:    timeout,
		})

		// Decay over 3 minutes
		for min := 1; min <= 3; min++ {
			decayFactor := 1.0 - (0.25 * float64(min))
			currentRPM := bauRPM * (1.0 + 3.0*decayFactor)
			currentErrors := max(0.08*decayFactor, healthyErrorRate*degradation)

			specs = append(specs, loadgen.LoadSpec{
				RPM:          int(currentRPM),
				ErrorRate:    currentErrors,
				DurationS:    60,
				P50LatencyMS: nominalP50 * degradation * (1 + decayFactor),
				P99LatencyMS: nominalP99 * degradation * (1 + 2*decayFactor),
				TimeoutMS:    timeout,
			})
		}

		// BAU for rest of hour - degradation creeping up
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM),
			ErrorRate:    healthyErrorRate * degradation,
			DurationS:    55 * 60, // 55 minutes - total hour = 2+3+55 = 60 min
			P50LatencyMS: nominalP50 * degradation,
			P99LatencyMS: nominalP99 * degradation,
			TimeoutMS:    timeout,
		})
	}

	// Hour 18 (6 PM): Second mega-spike with degraded DB
	const hour18BAU = 8.0
	const hour18BauRPM = hour18BAU * float64(baselineRPM)
	hour18Degradation := bauDegradation(18) // 1.5x at hour 18

	// Initial 6 PM spike - bad but not worst yet
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 4.5), // 36x original - initial surge
		ErrorRate:    0.20,                    // 20% errors
		DurationS:    120,                     // 2 minutes
		P50LatencyMS: nominalP50 * hour18Degradation * 2.0,
		P99LatencyMS: nominalP99 * hour18Degradation * 2.5,
		TimeoutMS:    timeout,
	})

	// Retry storm - things get WORSE
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 6.0), // 48x original - retry storm peaks
		ErrorRate:    0.30,                    // 30% errors - worse
		DurationS:    180,                     // 3 minutes
		P50LatencyMS: nominalP50 * hour18Degradation * 2.5,
		P99LatencyMS: nominalP99 * hour18Degradation * 3.0,
		TimeoutMS:    timeout,
	})

	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM * 2.0),
		ErrorRate:    0.05,
		DurationS:    600, // 10 minutes
		P50LatencyMS: nominalP50 * hour18Degradation * 1.5,
		P99LatencyMS: nominalP99 * hour18Degradation * 2.0,
		TimeoutMS:    timeout,
	})

	// Rest of hour 18
	specs = append(specs, loadgen.LoadSpec{
		RPM:          int(hour18BauRPM),
		ErrorRate:    healthyErrorRate * hour18Degradation,
		DurationS:    45 * 60, // 45 minutes
		P50LatencyMS: nominalP50 * hour18Degradation,
		P99LatencyMS: nominalP99 * hour18Degradation,
		TimeoutMS:    timeout,
	})

	// Hours 19-23: Wind down from 8x to 2x, degradation continues to creep (1.5x -> 3x)
	for hour := 19; hour <= 23; hour++ {
		bauTraffic := 8.0 - (6.0 * float64(hour-19) / 5.0) // 8x -> 2x
		degradation := bauDegradation(hour)
		bauRPM := bauTraffic * float64(baselineRPM)

		// Spikes with a few retries
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM * 3.0), // Full spike with retries
			ErrorRate:    0.05,              // 5% errors - worse
			DurationS:    120,
			P50LatencyMS: nominalP50 * degradation * 1.5,
			P99LatencyMS: nominalP99 * degradation * 2.0,
			TimeoutMS:    timeout,
		})

		// Decay over 3 minutes
		for min := 1; min <= 3; min++ {
			decayFactor := 1.0 - (0.25 * float64(min))
			currentRPM := bauRPM * (1.0 + 2.0*decayFactor)
			currentErrors := max(0.05*decayFactor, healthyErrorRate*degradation)

			specs = append(specs, loadgen.LoadSpec{
				RPM:          int(currentRPM),
				ErrorRate:    currentErrors,
				DurationS:    60,
				P50LatencyMS: nominalP50 * degradation * (1 + 0.5*decayFactor),
				P99LatencyMS: nominalP99 * degradation * (1 + decayFactor),
				TimeoutMS:    timeout,
			})
		}

		// BAU
		specs = append(specs, loadgen.LoadSpec{
			RPM:          int(bauRPM),
			ErrorRate:    healthyErrorRate * degradation,
			DurationS:    55 * 60, // 55 minutes - total hour = 2+3+55 = 60 min
			P50LatencyMS: nominalP50 * degradation,
			P99LatencyMS: nominalP99 * degradation,
			TimeoutMS:    timeout,
		})
	}

	return specs
}

func stateString(s levee.State) string {
	switch s {
	case levee.INIT:
		return "INIT"
	case levee.CLOSED:
		return "CLOSED"
	case levee.OPEN:
		return "OPEN"
	case levee.HALF_OPEN:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}
