package benchmarks

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/loadgen"
)

// PrescientBreaker knows the future from load specs and makes perfect decisions
type PrescientBreaker struct {
	slo       levee.SLO
	specs     []loadgen.LoadSpec
	startTime time.Time
}

func NewPrescientBreaker(slo levee.SLO, specs []loadgen.LoadSpec, startTime time.Time) *PrescientBreaker {
	return &PrescientBreaker{
		slo:       slo,
		specs:     specs,
		startTime: startTime,
	}
}

// State returns the ideal state at the given timestamp based on load specs
func (pb *PrescientBreaker) State(ts time.Time) levee.State {
	elapsed := ts.Sub(pb.startTime)

	// Skip warmup period - stay INIT
	if elapsed < pb.slo.Warmup {
		return levee.INIT
	}

	// After warmup, transition to CLOSED
	if elapsed < pb.slo.Warmup+time.Second {
		return levee.CLOSED
	}

	// Find current spec
	totalDuration := time.Duration(0)
	for _, spec := range pb.specs {
		specDuration := time.Duration(spec.DurationS) * time.Second
		if elapsed < totalDuration+specDuration {
			// We're in this spec - check if it's healthy according to SLO
			if spec.ErrorRate > (1.0 - pb.slo.SuccessRate) {
				return levee.OPEN // Backend is unhealthy
			}
			return levee.CLOSED // Backend is healthy
		}
		totalDuration += specDuration
	}

	return levee.CLOSED // Default to closed
}

type StateTransitionWithPrescient struct {
	Timestamp      time.Time
	LeveeState     levee.State
	PrescientState levee.State
	Category       string // "false_alarm", "slow_recovery", "late_detection", "proper_recovery"
}

// PrescientMetrics tracks circuit breaker performance against prescient breaker
type PrescientMetrics struct {
	// First state changes
	PrescientFirstOpenTime   time.Time
	PrescientFirstCloseTime  time.Time
	PrescientLastCloseTime   time.Time // Track most recent prescient close for flapping detection
	LeveeFirstOpenTime       time.Time
	LeveeFirstCloseAfterOpen time.Time

	// Lag metrics
	OpeningLag time.Duration // How long after prescient opens does Levee open
	ClosingLag time.Duration // How long after prescient closes does Levee close

	// Request deltas (compared to prescient)
	ExtraBadRequestsAllowed  int64 // Bad requests Levee allowed that prescient blocked
	ExtraGoodRequestsBlocked int64 // Good requests Levee blocked that prescient allowed

	// State transition categorization
	FalseAlarms               int64 // Levee opened when Prescient was CLOSED (unnecessary trip)
	FalseAlarmsDuringRecovery int64 // Subset: false alarms within 1 min of Prescient closing (flapping)
	FalseAlarmsOutOfBlue      int64 // Subset: false alarms when backend has been healthy for a while
	SlowRecoveries            int64 // Levee closed when Prescient was already CLOSED (lag in recovery)
	LateDetections            int64 // Levee opened when Prescient was already OPEN (lag in detection)
	PrematureRecoveries       int64 // Levee closed when Prescient was OPEN (premature recovery - risky!)

	// Track all Levee state transitions for analysis
	LeveeStateTransitions []StateTransitionWithPrescient

	// Totals for context
	TotalRequests    int64
	TotalSuccesses   int64
	TotalFailures    int64
	LeveeAllowed     int64
	LeveeBlocked     int64
	PrescientAllowed int64
	PrescientBlocked int64

	// Timing
	StartTime time.Time
	EndTime   time.Time
}

func NewPrescientMetrics() *PrescientMetrics {
	return &PrescientMetrics{
		LeveeStateTransitions: make([]StateTransitionWithPrescient, 0),
	}
}

func (m *PrescientMetrics) Finalize(endTime time.Time) {
	m.EndTime = endTime

	// Calculate lags
	if !m.PrescientFirstOpenTime.IsZero() && !m.LeveeFirstOpenTime.IsZero() {
		m.OpeningLag = m.LeveeFirstOpenTime.Sub(m.PrescientFirstOpenTime)
	}

	if !m.PrescientFirstCloseTime.IsZero() && !m.LeveeFirstCloseAfterOpen.IsZero() {
		m.ClosingLag = m.LeveeFirstCloseAfterOpen.Sub(m.PrescientFirstCloseTime)
	}
}

// CandidateResult holds benchmark results for a single candidate
type CandidateResult struct {
	Name    string
	Metrics *PrescientMetrics
}

// BenchmarkCyberMondayPrescient runs benchmark against prescient breaker for all candidates
func BenchmarkCyberMondayPrescient(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
		Warmup:      10 * time.Second,
	}

	specs := generateCyberMondayWorkload()

	b.Logf("Generated %d load specifications for Cyber Monday simulation", len(specs))

	// Run benchmark for each candidate
	candidates := []struct {
		name    string
		breaker levee.ICircuitBreaker
	}{
		{"Levee", levee.NewLevee(slo)},
		{"Static-BAU", NewStaticBAU()},
		{"Static-Peak", NewStaticPeak()},
	}

	results := make([]CandidateResult, 0, len(candidates))

	for _, c := range candidates {
		b.Logf("\n>>> Running benchmark for %s...", c.name)
		metrics := runCandidateBenchmark(slo, specs, c.breaker)
		results = append(results, CandidateResult{Name: c.name, Metrics: metrics})
		reportPrescientResults(b, c.name+" vs Prescient", metrics)
	}

	// Report comparative summary
	reportComparativeSummary(b, results)
}

// runCandidateBenchmark runs the benchmark for a single ICircuitBreaker candidate
func runCandidateBenchmark(slo levee.SLO, specs []loadgen.LoadSpec, breaker levee.ICircuitBreaker) *PrescientMetrics {
	metrics := NewPrescientMetrics()

	gen := loadgen.NewLoadGenerator(specs)
	stream := loadgen.NewEventStream(gen)

	var prescient *PrescientBreaker

	type pendingRequest struct {
		startTime         time.Time
		groundTruthResult bool
		breakerAllowed    bool
		prescientAllowed  bool
		breakerState      levee.State
		prescientState    levee.State
	}
	pending := make(map[int64]pendingRequest)

	breakerHasOpened := false
	var lastEvent loadgen.SimEvent
	var prevBreakerState levee.State = levee.INIT
	var prevPrescientState levee.State = levee.INIT

	for {
		event, err := stream.Next()
		if err != nil {
			break
		}
		lastEvent = event

		if metrics.StartTime.IsZero() {
			metrics.StartTime = event.Timestamp
			prescient = NewPrescientBreaker(slo, specs, event.Timestamp)
		}

		switch event.Status {
		case loadgen.EventStart:
			breakerState, breakerErr := breaker.Start(event.Timestamp)
			prescientState := prescient.State(event.Timestamp)

			if prescientState != prevPrescientState {
				if prescientState == levee.CLOSED && prevPrescientState == levee.OPEN {
					metrics.PrescientLastCloseTime = event.Timestamp
				}
				prevPrescientState = prescientState
			}

			if breakerState != prevBreakerState {
				var category string

				if breakerState == levee.OPEN && (prevBreakerState == levee.CLOSED || prevBreakerState == levee.INIT) {
					if prescientState == levee.CLOSED {
						category = "false_alarm"
						metrics.FalseAlarms++

						if !metrics.PrescientLastCloseTime.IsZero() {
							timeSinceClose := event.Timestamp.Sub(metrics.PrescientLastCloseTime)
							if timeSinceClose < 1*time.Minute {
								metrics.FalseAlarmsDuringRecovery++
							} else {
								metrics.FalseAlarmsOutOfBlue++
							}
						} else {
							metrics.FalseAlarmsOutOfBlue++
						}
					} else if prescientState == levee.OPEN {
						category = "late_detection"
						metrics.LateDetections++
					}
				}

				if breakerState == levee.CLOSED && prevBreakerState != levee.CLOSED && prevBreakerState != levee.INIT {
					if prescientState == levee.CLOSED {
						category = "slow_recovery"
						metrics.SlowRecoveries++
					} else if prescientState == levee.OPEN {
						category = "premature_recovery"
						metrics.PrematureRecoveries++
					}
				}

				if category != "" {
					metrics.LeveeStateTransitions = append(metrics.LeveeStateTransitions, StateTransitionWithPrescient{
						Timestamp:      event.Timestamp,
						LeveeState:     breakerState,
						PrescientState: prescientState,
						Category:       category,
					})
				}

				prevBreakerState = breakerState
			}

			if prescientState == levee.OPEN && metrics.PrescientFirstOpenTime.IsZero() {
				metrics.PrescientFirstOpenTime = event.Timestamp
			}
			if breakerState == levee.OPEN && metrics.LeveeFirstOpenTime.IsZero() {
				metrics.LeveeFirstOpenTime = event.Timestamp
				breakerHasOpened = true
			}

			if breakerHasOpened && prescientState == levee.CLOSED && metrics.PrescientFirstCloseTime.IsZero() {
				metrics.PrescientFirstCloseTime = event.Timestamp
			}

			if breakerHasOpened && breakerState == levee.CLOSED && metrics.LeveeFirstCloseAfterOpen.IsZero() {
				metrics.LeveeFirstCloseAfterOpen = event.Timestamp
			}

			breakerAllowed := (breakerErr == nil)
			prescientAllowed := (prescientState != levee.OPEN)

			if breakerAllowed {
				metrics.LeveeAllowed++
			} else {
				metrics.LeveeBlocked++
			}

			if prescientAllowed {
				metrics.PrescientAllowed++
			} else {
				metrics.PrescientBlocked++
			}

			pending[event.EventID] = pendingRequest{
				startTime:        event.Timestamp,
				breakerAllowed:   breakerAllowed,
				prescientAllowed: prescientAllowed,
				breakerState:     breakerState,
				prescientState:   prescientState,
			}

		case loadgen.EventSuccess:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)

			metrics.TotalRequests++
			metrics.TotalSuccesses++

			if req.breakerAllowed {
				breaker.Success(event.Timestamp, duration)
			}

			if req.prescientAllowed && !req.breakerAllowed {
				metrics.ExtraGoodRequestsBlocked++
			}

			delete(pending, event.EventID)

		case loadgen.EventError:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)

			metrics.TotalRequests++
			metrics.TotalFailures++

			if req.breakerAllowed {
				breaker.Fail(event.Timestamp, duration)
			}

			if !req.prescientAllowed && req.breakerAllowed {
				metrics.ExtraBadRequestsAllowed++
			}

			delete(pending, event.EventID)
		}
	}

	metrics.Finalize(lastEvent.Timestamp)
	return metrics
}

// reportComparativeSummary prints a comparison of all candidates
func reportComparativeSummary(b *testing.B, results []CandidateResult) {
	sep := strings.Repeat("=", 100)
	b.Logf("\n%s", sep)
	b.Logf("  COMPARATIVE SUMMARY - All Candidates vs Prescient (Ideal)")
	b.Logf("%s", sep)

	// Find prescient baseline (first result's prescient metrics)
	if len(results) == 0 {
		return
	}
	prescientBlocked := results[0].Metrics.PrescientBlocked
	prescientAllowed := results[0].Metrics.PrescientAllowed

	b.Logf("\n%-15s | %12s | %12s | %10s | %10s | %10s | %12s | %12s",
		"Candidate", "Blocked", "Allowed", "FalseAlarm", "LateDetect", "SlowRecov", "ExtraBad", "ExtraGood")
	b.Logf("%s", strings.Repeat("-", 110))
	b.Logf("%-15s | %12d | %12d | %10s | %10s | %10s | %12s | %12s",
		"Prescient", prescientBlocked, prescientAllowed, "0", "0", "0", "0", "0")

	for _, r := range results {
		m := r.Metrics
		b.Logf("%-15s | %12d | %12d | %10d | %10d | %10d | %12d | %12d",
			r.Name, m.LeveeBlocked, m.LeveeAllowed, m.FalseAlarms, m.LateDetections, m.SlowRecoveries,
			m.ExtraBadRequestsAllowed, m.ExtraGoodRequestsBlocked)
	}

	// Calculate relative performance
	b.Logf("\n--- Relative Performance (lower is better) ---")
	b.Logf("%-15s | %12s | %12s | %12s", "Candidate", "Block Delta", "Allow Delta", "Decision Err")
	b.Logf("%s", strings.Repeat("-", 60))

	for _, r := range results {
		m := r.Metrics
		blockDelta := m.LeveeBlocked - prescientBlocked
		allowDelta := m.LeveeAllowed - prescientAllowed
		decisionErr := m.ExtraBadRequestsAllowed + m.ExtraGoodRequestsBlocked
		b.Logf("%-15s | %+12d | %+12d | %12d", r.Name, blockDelta, allowDelta, decisionErr)
	}

	b.Logf("\n%s", sep)
}

func runPrescientBenchmark(slo levee.SLO, specs []loadgen.LoadSpec) *PrescientMetrics {
	lev := levee.NewLevee(slo)
	metrics := NewPrescientMetrics()

	gen := loadgen.NewLoadGenerator(specs)
	stream := loadgen.NewEventStream(gen)

	// Create prescient breaker
	var prescient *PrescientBreaker

	// Track pending requests: EventID -> ground truth outcome
	type pendingRequest struct {
		startTime         time.Time
		groundTruthResult bool // true = success, false = failure
		leveeAllowed      bool
		prescientAllowed  bool
		leveeState        levee.State
		prescientState    levee.State
	}
	pending := make(map[int64]pendingRequest)

	leveeHasOpened := false
	var lastEvent loadgen.SimEvent
	var prevLeveeState levee.State = levee.INIT
	var prevPrescientState levee.State = levee.INIT

	for {
		event, err := stream.Next()
		if err != nil {
			break
		}
		lastEvent = event

		if metrics.StartTime.IsZero() {
			metrics.StartTime = event.Timestamp
			prescient = NewPrescientBreaker(slo, specs, event.Timestamp)
		}

		switch event.Status {
		case loadgen.EventStart:
			// Get states
			leveeState, leveeErr := lev.Start(event.Timestamp)
			prescientState := prescient.State(event.Timestamp)

			// Track prescient state changes to detect when it closes
			if prescientState != prevPrescientState {
				if prescientState == levee.CLOSED && prevPrescientState == levee.OPEN {
					metrics.PrescientLastCloseTime = event.Timestamp
				}
				prevPrescientState = prescientState
			}

			// Track Levee state transitions and categorize them
			// Only track meaningful transitions: CLOSED->OPEN and OPEN->CLOSED
			if leveeState != prevLeveeState {
				var category string

				// Track CLOSED -> OPEN (opening the circuit)
				if leveeState == levee.OPEN && (prevLeveeState == levee.CLOSED || prevLeveeState == levee.INIT) {
					if prescientState == levee.CLOSED {
						category = "false_alarm"
						metrics.FalseAlarms++

						// Categorize false alarm: recovery flapping vs out of blue
						if !metrics.PrescientLastCloseTime.IsZero() {
							timeSinceClose := event.Timestamp.Sub(metrics.PrescientLastCloseTime)
							if timeSinceClose < 1*time.Minute {
								// Within 1 minute of prescient closing - likely flapping during recovery
								metrics.FalseAlarmsDuringRecovery++
							} else {
								// Been healthy for a while - trip out of blue
								metrics.FalseAlarmsOutOfBlue++
							}
						} else {
							// Prescient hasn't even opened yet - definitely out of blue
							metrics.FalseAlarmsOutOfBlue++
						}
					} else if prescientState == levee.OPEN {
						category = "late_detection"
						metrics.LateDetections++
					}
				}

				// Track OPEN -> CLOSED (closing the circuit, via HALF_OPEN)
				// Note: direct transition is OPEN -> HALF_OPEN -> CLOSED, so we track the final CLOSED
				if leveeState == levee.CLOSED && prevLeveeState != levee.CLOSED && prevLeveeState != levee.INIT {
					if prescientState == levee.CLOSED {
						category = "slow_recovery"
						metrics.SlowRecoveries++
					} else if prescientState == levee.OPEN {
						category = "premature_recovery" // risky!
						metrics.PrematureRecoveries++
					}
				}

				if category != "" {
					metrics.LeveeStateTransitions = append(metrics.LeveeStateTransitions, StateTransitionWithPrescient{
						Timestamp:      event.Timestamp,
						LeveeState:     leveeState,
						PrescientState: prescientState,
						Category:       category,
					})
				}

				prevLeveeState = leveeState
			}

			// Track first opens
			if prescientState == levee.OPEN && metrics.PrescientFirstOpenTime.IsZero() {
				metrics.PrescientFirstOpenTime = event.Timestamp
			}
			if leveeState == levee.OPEN && metrics.LeveeFirstOpenTime.IsZero() {
				metrics.LeveeFirstOpenTime = event.Timestamp
				leveeHasOpened = true
			}

			// Track first close after open for prescient
			if leveeHasOpened && prescientState == levee.CLOSED && metrics.PrescientFirstCloseTime.IsZero() {
				metrics.PrescientFirstCloseTime = event.Timestamp
			}

			// Track first close after open for levee
			if leveeHasOpened && leveeState == levee.CLOSED && metrics.LeveeFirstCloseAfterOpen.IsZero() {
				metrics.LeveeFirstCloseAfterOpen = event.Timestamp
			}

			leveeAllowed := (leveeErr == nil)
			prescientAllowed := (prescientState != levee.OPEN)

			if leveeAllowed {
				metrics.LeveeAllowed++
			} else {
				metrics.LeveeBlocked++
			}

			if prescientAllowed {
				metrics.PrescientAllowed++
			} else {
				metrics.PrescientBlocked++
			}

			// Store for later comparison
			pending[event.EventID] = pendingRequest{
				startTime:        event.Timestamp,
				leveeAllowed:     leveeAllowed,
				prescientAllowed: prescientAllowed,
				leveeState:       leveeState,
				prescientState:   prescientState,
			}

		case loadgen.EventSuccess:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)

			metrics.TotalRequests++
			metrics.TotalSuccesses++

			// Update ground truth
			req.groundTruthResult = true
			pending[event.EventID] = req

			// Report to Levee
			if req.leveeAllowed {
				lev.Success(event.Timestamp, duration)
			}

			// Compare against prescient
			// If prescient allowed but Levee blocked, that's an extra good request blocked
			if req.prescientAllowed && !req.leveeAllowed {
				metrics.ExtraGoodRequestsBlocked++
			}

			delete(pending, event.EventID)

		case loadgen.EventError:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)

			metrics.TotalRequests++
			metrics.TotalFailures++

			// Update ground truth
			req.groundTruthResult = false
			pending[event.EventID] = req

			// Report to Levee
			if req.leveeAllowed {
				lev.Fail(event.Timestamp, duration)
			}

			// Compare against prescient
			// If prescient blocked but Levee allowed, that's an extra bad request allowed
			if !req.prescientAllowed && req.leveeAllowed {
				metrics.ExtraBadRequestsAllowed++
			}

			delete(pending, event.EventID)
		}
	}

	metrics.Finalize(lastEvent.Timestamp)
	return metrics
}

func reportPrescientResults(b *testing.B, name string, m *PrescientMetrics) {
	// Save full transition log to file
	saveTransitionLog(m)

	sep := strings.Repeat("=", 80)
	b.Logf("\n%s", sep)
	b.Logf("  %s - Cyber Monday Benchmark Results", name)
	b.Logf("%s", sep)

	b.Logf("\n--- Overview ---")
	b.Logf("Total Requests:       %d", m.TotalRequests)
	b.Logf("Total Successes:      %d (%.2f%%)", m.TotalSuccesses,
		float64(m.TotalSuccesses)/float64(m.TotalRequests)*100)
	b.Logf("Total Failures:       %d (%.2f%%)", m.TotalFailures,
		float64(m.TotalFailures)/float64(m.TotalRequests)*100)

	b.Logf("\n--- Breaker Decisions ---")
	b.Logf("Levee Allowed:        %d (%.2f%%)", m.LeveeAllowed,
		float64(m.LeveeAllowed)/float64(m.TotalRequests)*100)
	b.Logf("Levee Blocked:        %d (%.2f%%)", m.LeveeBlocked,
		float64(m.LeveeBlocked)/float64(m.TotalRequests)*100)
	b.Logf("Prescient Allowed:    %d (%.2f%%)", m.PrescientAllowed,
		float64(m.PrescientAllowed)/float64(m.TotalRequests)*100)
	b.Logf("Prescient Blocked:    %d (%.2f%%)", m.PrescientBlocked,
		float64(m.PrescientBlocked)/float64(m.TotalRequests)*100)

	b.Logf("\n--- Detection Lag (Key Metric) ---")
	b.Logf("Prescient First Open: %v", m.PrescientFirstOpenTime.Sub(m.StartTime))
	b.Logf("Levee First Open:     %v", m.LeveeFirstOpenTime.Sub(m.StartTime))
	b.Logf("Opening Lag:          %v", m.OpeningLag)
	if !m.PrescientFirstCloseTime.IsZero() {
		b.Logf("Prescient First Close:%v", m.PrescientFirstCloseTime.Sub(m.StartTime))
	}
	if !m.LeveeFirstCloseAfterOpen.IsZero() {
		b.Logf("Levee First Close:    %v", m.LeveeFirstCloseAfterOpen.Sub(m.StartTime))
	}
	if m.ClosingLag > 0 {
		b.Logf("Closing Lag:          %v", m.ClosingLag)
	}

	b.Logf("\n--- Error Delta (Key Metric) ---")
	b.Logf("Extra Bad Requests Allowed:  %d (Levee allowed, prescient blocked)", m.ExtraBadRequestsAllowed)
	b.Logf("Extra Good Requests Blocked: %d (Levee blocked, prescient allowed)", m.ExtraGoodRequestsBlocked)

	b.Logf("\n--- State Transition Analysis ---")
	b.Logf("False Alarms:      %d (Levee opened when Prescient CLOSED - unnecessary)", m.FalseAlarms)
	b.Logf("  During Recovery: %d (within 1min of Prescient closing - flapping)", m.FalseAlarmsDuringRecovery)
	b.Logf("  Out of Blue:     %d (backend healthy, Levee trips on load spikes)", m.FalseAlarmsOutOfBlue)
	b.Logf("Late Detections:   %d (Levee opened when Prescient already OPEN - slow)", m.LateDetections)
	b.Logf("Slow Recoveries:   %d (Levee closed when Prescient already CLOSED - lag)", m.SlowRecoveries)
	b.Logf("Premature Recov:   %d (Levee closed when Prescient still OPEN - risky!)", m.PrematureRecoveries)

	// Show first few transitions for debugging
	if len(m.LeveeStateTransitions) > 0 {
		b.Logf("\nFirst 10 state transitions:")
		limit := 10
		if len(m.LeveeStateTransitions) < limit {
			limit = len(m.LeveeStateTransitions)
		}
		for i := 0; i < limit; i++ {
			t := m.LeveeStateTransitions[i]
			elapsed := t.Timestamp.Sub(m.StartTime)
			b.Logf("  %v: Levee=%s, Prescient=%s → %s",
				elapsed, stateString(t.LeveeState), stateString(t.PrescientState), t.Category)
		}
	}

	// Analyze false alarm distribution over time
	if m.FalseAlarmsOutOfBlue > 0 {
		b.Logf("\n--- False Alarm Distribution (Out of Blue only) ---")

		// Group by hour
		hourlyFalseAlarms := make(map[int]int)
		for _, t := range m.LeveeStateTransitions {
			if t.Category == "false_alarm" {
				// Check if it's out of blue
				timeSinceClose := time.Duration(0)
				if !m.PrescientLastCloseTime.IsZero() {
					timeSinceClose = t.Timestamp.Sub(m.PrescientLastCloseTime)
				}
				isOutOfBlue := m.PrescientLastCloseTime.IsZero() || timeSinceClose >= 1*time.Minute

				if isOutOfBlue {
					elapsed := t.Timestamp.Sub(m.StartTime)
					hour := int(elapsed.Hours())
					hourlyFalseAlarms[hour]++
				}
			}
		}

		// Print hourly distribution
		for hour := 0; hour < 30; hour++ {
			if count, ok := hourlyFalseAlarms[hour]; ok {
				b.Logf("  Hour %2d: %d false alarms", hour, count)
			}
		}
	}

	totalDuration := m.EndTime.Sub(m.StartTime)
	b.Logf("\nTotal Duration: %v", totalDuration)
	b.Logf("%s", sep)
}

func saveTransitionLog(m *PrescientMetrics) {
	f, err := os.Create("transition_log.txt")
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Full Transition Log ===\n")
	fmt.Fprintf(f, "Total transitions: %d\n", len(m.LeveeStateTransitions))
	fmt.Fprintf(f, "Start Time: %v\n", m.StartTime)
	fmt.Fprintf(f, "End Time: %v\n", m.EndTime)
	fmt.Fprintf(f, "Total Duration: %v\n\n", m.EndTime.Sub(m.StartTime))

	for i, t := range m.LeveeStateTransitions {
		elapsed := t.Timestamp.Sub(m.StartTime)
		hour := int(elapsed.Hours())
		fmt.Fprintf(f, "[%4d] Hour %2d | %12v | Levee=%-10s | Prescient=%-10s | %s\n",
			i+1, hour, elapsed, stateString(t.LeveeState), stateString(t.PrescientState), t.Category)
	}

	fmt.Fprintf(f, "\n=== Summary by Hour ===\n")
	hourlyStats := make(map[int]map[string]int)
	for _, t := range m.LeveeStateTransitions {
		elapsed := t.Timestamp.Sub(m.StartTime)
		hour := int(elapsed.Hours())
		if hourlyStats[hour] == nil {
			hourlyStats[hour] = make(map[string]int)
		}
		hourlyStats[hour][t.Category]++
	}

	for hour := 0; hour < 30; hour++ {
		if stats, ok := hourlyStats[hour]; ok {
			fmt.Fprintf(f, "Hour %2d: ", hour)
			for cat, count := range stats {
				fmt.Fprintf(f, "%s=%d ", cat, count)
			}
			fmt.Fprintf(f, "\n")
		}
	}

	// Write JSON version with all metrics for detailed analysis
	jsonFile, err := os.Create("transition_log.json")
	if err == nil {
		defer jsonFile.Close()
		enc := json.NewEncoder(jsonFile)
		enc.SetIndent("", "  ")
		enc.Encode(m)
	}
}

// buildLeveeStateUntil feeds events to Levee until the specified time offset
func buildLeveeStateUntil(slo levee.SLO, specs []loadgen.LoadSpec, untilOffset time.Duration) *levee.Levee {
	lev := levee.NewLevee(slo)
	gen := loadgen.NewLoadGenerator(specs)
	stream := loadgen.NewEventStream(gen)

	type pendingRequest struct {
		startTime time.Time
	}
	pending := make(map[int64]pendingRequest)

	var startTime time.Time

	for {
		event, err := stream.Next()
		if err != nil {
			break
		}

		if startTime.IsZero() {
			startTime = event.Timestamp
		}

		elapsed := event.Timestamp.Sub(startTime)
		if elapsed >= untilOffset {
			break
		}

		switch event.Status {
		case loadgen.EventStart:
			if _, err := lev.Start(event.Timestamp); err == nil {
				pending[event.EventID] = pendingRequest{startTime: event.Timestamp}
			}
		case loadgen.EventSuccess:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)
			if _, ok := pending[event.EventID]; ok {
				lev.Success(event.Timestamp, duration)
				delete(pending, event.EventID)
			}
		case loadgen.EventError:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)
			if _, ok := pending[event.EventID]; ok {
				lev.Fail(event.Timestamp, duration)
				delete(pending, event.EventID)
			}
		}
	}

	return lev
}

// BenchmarkGenerateLeveeState builds Levee state up to h+03:59 and saves to file
func BenchmarkGenerateLeveeState(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
		Warmup:      10 * time.Second,
	}

	specs := generateCyberMondayWorkload()

	b.Logf("Building Levee state until h+03:59...")
	leveeAtCutoff := buildLeveeStateUntil(slo, specs, 3*time.Hour+59*time.Minute)

	b.Logf("Levee state at h+03:59: %s", stateString(leveeAtCutoff.State()))

	savedState, err := leveeAtCutoff.SaveState()
	if err != nil {
		b.Fatalf("Failed to save Levee state: %v", err)
	}
	if savedState == nil {
		b.Fatalf("Saved state is nil - Levee may not be in CLOSED state or still in warmup")
	}

	// Save to file
	data, err := json.MarshalIndent(savedState, "", "  ")
	if err != nil {
		b.Fatalf("Failed to marshal state: %v", err)
	}

	err = os.WriteFile("levee_state_h03_59.json", data, 0644)
	if err != nil {
		b.Fatalf("Failed to write state file: %v", err)
	}

	b.Logf("Successfully saved Levee state to levee_state_h03_59.json")
}

// BenchmarkGenerateStateForFalseAlarmDebug builds Levee state up to h+04:59 for debugging false alarms
func BenchmarkGenerateStateForFalseAlarmDebug(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
		Warmup:      10 * time.Second,
	}

	specs := generateCyberMondayWorkload()

	b.Logf("Building Levee state until h+04:59...")
	leveeAtCutoff := buildLeveeStateUntil(slo, specs, 4*time.Hour+59*time.Minute)

	b.Logf("Levee state at h+04:59: %s", stateString(leveeAtCutoff.State()))

	savedState, err := leveeAtCutoff.SaveState()
	if err != nil {
		b.Fatalf("Failed to save Levee state: %v", err)
	}
	if savedState == nil {
		b.Fatalf("Saved state is nil - Levee may not be in CLOSED state or still in warmup")
	}

	// Save to file
	data, err := json.MarshalIndent(savedState, "", "  ")
	if err != nil {
		b.Fatalf("Failed to marshal state: %v", err)
	}

	err = os.WriteFile("levee_state_h04_59.json", data, 0644)
	if err != nil {
		b.Fatalf("Failed to write state file: %v", err)
	}

	b.Logf("Successfully saved Levee state to levee_state_h04_59.json")
}

// BenchmarkFalseAlarmDebug runs a focused benchmark around h+05:00 to h+05:30 where first false alarms occur
func BenchmarkFalseAlarmDebug(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
		Warmup:      10 * time.Second,
	}

	specs := generateCyberMondayWorkload()

	// Load saved state from file
	data, err := os.ReadFile("levee_state_h04_59.json")
	if err != nil {
		b.Fatalf("Failed to read state file: %v. Run BenchmarkGenerateStateForFalseAlarmDebug first.", err)
	}

	var savedState levee.LeveeState
	err = json.Unmarshal(data, &savedState)
	if err != nil {
		b.Fatalf("Failed to unmarshal state: %v", err)
	}

	b.Logf("Loaded Levee state from file")
	b.Logf("State buffer size: %d, Error EWMA base: %.4f", savedState.BufferSize, savedState.ErrorsValueBase)

	// Run the truncated benchmark from h+05:00 to h+05:30 (where first false alarms occur)
	metrics := runTruncatedPrescientBenchmark(b, slo, specs, &savedState, 5*time.Hour, 5*time.Hour+30*time.Minute)
	reportPrescientResults(b, "Levee vs Prescient (h+05:00 to h+05:30) - False Alarm Debug", metrics)
}

// BenchmarkCyberMondayPrescientTruncated runs truncated benchmark from h+04:00 to h+04:10
func BenchmarkTruncated(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
		Warmup:      10 * time.Second,
	}

	specs := generateCyberMondayWorkload()

	// Load saved state from file
	data, err := os.ReadFile("levee_state_h03_59.json")
	if err != nil {
		b.Fatalf("Failed to read state file: %v. Run BenchmarkGenerateLeveeState first.", err)
	}

	var savedState levee.LeveeState
	err = json.Unmarshal(data, &savedState)
	if err != nil {
		b.Fatalf("Failed to unmarshal state: %v", err)
	}

	b.Logf("Loaded Levee state from file")

	// Run the truncated benchmark
	metrics := runTruncatedPrescientBenchmark(b, slo, specs, &savedState, 4*time.Hour, 4*time.Hour+20*time.Minute)
	reportPrescientResults(b, "Levee vs Prescient (h+04:00 to h+04:20)", metrics)
}

// runTruncatedPrescientBenchmark runs benchmark in a specific time window using restored state
func runTruncatedPrescientBenchmark(b *testing.B, slo levee.SLO, specs []loadgen.LoadSpec, savedState *levee.LeveeState, startOffset, endOffset time.Duration) *PrescientMetrics {
	lev := levee.RestoreState(savedState)

	metrics := NewPrescientMetrics()
	gen := loadgen.NewLoadGenerator(specs)
	stream := loadgen.NewEventStream(gen)

	var prescient *PrescientBreaker
	var simulationStartTime time.Time

	type pendingRequest struct {
		startTime        time.Time
		leveeAllowed     bool
		prescientAllowed bool
	}
	pending := make(map[int64]pendingRequest)

	leveeHasOpened := false
	var lastEvent loadgen.SimEvent
	var prevLeveeState levee.State = lev.State()
	var prevPrescientState levee.State = levee.INIT

	// Skip to start offset
	for {
		event, err := stream.Next()
		if err != nil {
			b.Fatalf("Reached end of stream before start offset")
		}

		if simulationStartTime.IsZero() {
			simulationStartTime = event.Timestamp
			prescient = NewPrescientBreaker(slo, specs, event.Timestamp)
			metrics.StartTime = event.Timestamp
		}

		elapsed := event.Timestamp.Sub(simulationStartTime)
		if elapsed >= startOffset {
			lastEvent = event
			break
		}
	}

	// Process events in the window
	for {
		event, err := stream.Next()
		if err != nil {
			break
		}
		lastEvent = event

		elapsed := event.Timestamp.Sub(simulationStartTime)
		if elapsed >= endOffset {
			break
		}

		switch event.Status {
		case loadgen.EventStart:
			leveeState, leveeErr := lev.Start(event.Timestamp)
			prescientState := prescient.State(event.Timestamp)

			if prescientState != prevPrescientState {
				if prescientState == levee.CLOSED && prevPrescientState == levee.OPEN {
					metrics.PrescientLastCloseTime = event.Timestamp
				}
				prevPrescientState = prescientState
			}

			if leveeState != prevLeveeState {
				var category string
				if leveeState == levee.OPEN && (prevLeveeState == levee.CLOSED || prevLeveeState == levee.INIT) {
					if prescientState == levee.CLOSED {
						category = "false_alarm"
						metrics.FalseAlarms++
					} else if prescientState == levee.OPEN {
						category = "late_detection"
						metrics.LateDetections++
					}
				}
				if leveeState == levee.CLOSED && prevLeveeState != levee.CLOSED && prevLeveeState != levee.INIT {
					if prescientState == levee.CLOSED {
						category = "slow_recovery"
						metrics.SlowRecoveries++
					} else if prescientState == levee.OPEN {
						category = "premature_recovery"
						metrics.PrematureRecoveries++
					}
				}
				if category != "" {
					metrics.LeveeStateTransitions = append(metrics.LeveeStateTransitions, StateTransitionWithPrescient{
						Timestamp:      event.Timestamp,
						LeveeState:     leveeState,
						PrescientState: prescientState,
						Category:       category,
					})
				}
				prevLeveeState = leveeState
			}

			if prescientState == levee.OPEN && metrics.PrescientFirstOpenTime.IsZero() {
				metrics.PrescientFirstOpenTime = event.Timestamp
			}
			if leveeState == levee.OPEN && metrics.LeveeFirstOpenTime.IsZero() {
				metrics.LeveeFirstOpenTime = event.Timestamp
				leveeHasOpened = true
			}
			if leveeHasOpened && prescientState == levee.CLOSED && metrics.PrescientFirstCloseTime.IsZero() {
				metrics.PrescientFirstCloseTime = event.Timestamp
			}
			if leveeHasOpened && leveeState == levee.CLOSED && metrics.LeveeFirstCloseAfterOpen.IsZero() {
				metrics.LeveeFirstCloseAfterOpen = event.Timestamp
			}

			leveeAllowed := (leveeErr == nil)
			prescientAllowed := (prescientState != levee.OPEN)

			if leveeAllowed {
				metrics.LeveeAllowed++
			} else {
				metrics.LeveeBlocked++
			}
			if prescientAllowed {
				metrics.PrescientAllowed++
			} else {
				metrics.PrescientBlocked++
			}

			pending[event.EventID] = pendingRequest{
				startTime:        event.Timestamp,
				leveeAllowed:     leveeAllowed,
				prescientAllowed: prescientAllowed,
			}

		case loadgen.EventSuccess:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)
			metrics.TotalRequests++
			metrics.TotalSuccesses++

			if req.leveeAllowed {
				lev.Success(event.Timestamp, duration)
			}
			if req.prescientAllowed && !req.leveeAllowed {
				metrics.ExtraGoodRequestsBlocked++
			}
			delete(pending, event.EventID)

		case loadgen.EventError:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)
			metrics.TotalRequests++
			metrics.TotalFailures++

			if req.leveeAllowed {
				lev.Fail(event.Timestamp, duration)
			}
			if !req.prescientAllowed && req.leveeAllowed {
				metrics.ExtraBadRequestsAllowed++
			}
			delete(pending, event.EventID)
		}
	}

	metrics.Finalize(lastEvent.Timestamp)
	return metrics
}
