package benchmarks

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codemartial/levee"
	"github.com/codemartial/loadgen"
)

// CircuitBreaker is a local interface for benchmark compatibility
type CircuitBreaker interface {
	Start(time.Time) (levee.StateChange, error)
	Success(time.Time, time.Duration) levee.StateChange
	Fail(time.Time, time.Duration) levee.StateChange
	State() levee.State
}

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
	Trigger        levee.Trigger
	Category       string // "false_alarm", "slow_recovery", "late_detection", "flapping"
}

// RawLeveeTransition captures a levee state change for post-processing
type RawLeveeTransition struct {
	Timestamp      time.Time
	FromState      levee.State
	ToState        levee.State
	Trigger        levee.Trigger
	PrescientState levee.State
}

// PrescientStateChange captures when prescient changes state
type PrescientStateChange struct {
	Timestamp time.Time
	ToState   levee.State
}

// pendingRequest tracks an in-flight request during benchmark
type pendingRequest struct {
	startTime        time.Time
	breakerAllowed   bool
	prescientAllowed bool
}

// RunningPenalty tracks RPS²-based penalties on-the-fly using 1-second windows.
// This avoids storing millions of request events in memory.
type RunningPenalty struct {
	currentSecond int64

	// Counts within current 1-second window
	badAllowedThisSecond  int // Requests allowed when Prescient OPEN (bad traffic)
	goodBlockedThisSecond int // Requests blocked when Prescient CLOSED (lost business)

	// Accumulated RPS² penalties (sum of count² per second)
	BadTrafficPenalty   float64 // Σ(badRPS²) - damage from letting bad traffic through
	LostBusinessPenalty float64 // Σ(blockedRPS²) - damage from blocking good traffic
}

// Record updates the penalty counters for a single request decision.
// Call this once per request at the Start event.
func (p *RunningPenalty) Record(ts time.Time, prescientOpen, cbAllowed bool) {
	sec := ts.Unix()

	// If we've moved to a new second, finalize the previous window
	if sec != p.currentSecond {
		p.finalizeWindow()
		p.currentSecond = sec
	}

	// Count this request in the appropriate bucket
	if prescientOpen && cbAllowed {
		p.badAllowedThisSecond++
	}
	if !prescientOpen && !cbAllowed {
		p.goodBlockedThisSecond++
	}
}

// finalizeWindow squares the counts and adds to running totals
func (p *RunningPenalty) finalizeWindow() {
	p.BadTrafficPenalty += float64(p.badAllowedThisSecond * p.badAllowedThisSecond)
	p.LostBusinessPenalty += float64(p.goodBlockedThisSecond * p.goodBlockedThisSecond)
	p.badAllowedThisSecond = 0
	p.goodBlockedThisSecond = 0
}

// Finalize should be called at the end of the benchmark to capture the last window
func (p *RunningPenalty) Finalize() {
	p.finalizeWindow()
}

// BenchmarkConfig provides optional parameters for benchmark runs
type BenchmarkConfig struct {
	StartOffset time.Duration // Skip events before this offset (0 = start from beginning)
	EndOffset   time.Duration // Stop at this offset (0 = run to end)
}

// PrescientMetrics tracks circuit breaker performance against prescient breaker
type PrescientMetrics struct {
	// First state changes
	PrescientFirstOpenTime   time.Time
	PrescientFirstCloseTime  time.Time
	LeveeFirstOpenTime       time.Time
	LeveeFirstCloseAfterOpen time.Time

	// Lag metrics
	OpeningLag time.Duration // How long after prescient opens does Levee open
	ClosingLag time.Duration // How long after prescient closes does Levee close

	// Request deltas (compared to prescient)
	ExtraBadRequestsAllowed  int64 // Bad requests Levee allowed that prescient blocked
	ExtraGoodRequestsBlocked int64 // Good requests Levee blocked that prescient allowed

	// RPS²-weighted penalties (computed on-the-fly)
	BadTrafficPenalty   float64 // Σ(RPS²) when Prescient OPEN and CB allowed requests
	LostBusinessPenalty float64 // Σ(RPS²) when Prescient CLOSED and CB blocked requests

	// State transition categorization (new definitions)
	FalseAlarms         int64 // Levee opened while Prescient stayed CLOSED within ±5s window
	LateDetections      int64 // Levee opened >1s after Prescient opened
	SlowRecoveries      int64 // Levee closed >5s after Prescient closed
	Flapping            int64 // Levee opened <1m after previous close
	PrematureRecoveries int64 // Levee closed when Prescient was still OPEN

	// Track all Levee state transitions for analysis
	LeveeStateTransitions []StateTransitionWithPrescient

	// Raw data for categorization (internal use)
	rawLeveeTransitions   []RawLeveeTransition
	prescientStateChanges []PrescientStateChange

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
		rawLeveeTransitions:   make([]RawLeveeTransition, 0),
		prescientStateChanges: make([]PrescientStateChange, 0),
	}
}

// effectiveState treats HALF_OPEN as OPEN for categorization purposes
func effectiveState(s levee.State) levee.State {
	if s == levee.HALF_OPEN {
		return levee.OPEN
	}
	return s
}

// recordBreakerStateChange records every breaker state change
// Returns the new prevState value to use
func recordBreakerStateChange(metrics *PrescientMetrics, sc levee.StateChange,
	prevState, prescientState levee.State, ts time.Time) levee.State {
	if sc.State != prevState {
		metrics.rawLeveeTransitions = append(metrics.rawLeveeTransitions, RawLeveeTransition{
			Timestamp:      ts,
			FromState:      prevState,
			ToState:        sc.State,
			Trigger:        sc.Trigger,
			PrescientState: prescientState,
		})
		return sc.State
	}
	return prevState
}

// recordPrescientStateChange records a prescient state change if it differs from previous
// Returns the new prevState value to use
func recordPrescientStateChange(metrics *PrescientMetrics, prevState, newState levee.State, ts time.Time) levee.State {
	if newState != prevState {
		metrics.prescientStateChanges = append(metrics.prescientStateChanges, PrescientStateChange{
			Timestamp: ts,
			ToState:   newState,
		})
		return newState
	}
	return prevState
}

// categorizeTransitions processes raw transitions and assigns categories based on time windows
// HALF_OPEN is treated as OPEN, and consecutive OPEN/HALF_OPEN events are squashed into one
func (m *PrescientMetrics) categorizeTransitions() {
	const (
		falseAlarmWindow   = 5 * time.Second // ±5s for false alarm detection
		lateDetectionDelay = 1 * time.Second // >1s after prescient opens
		slowRecoveryDelay  = 5 * time.Second // >5s after prescient closes
		flappingWindow     = 1 * time.Minute // <1m between close and open
	)

	var lastLeveeCloseTime time.Time
	var lastEffectiveState levee.State = levee.CLOSED

	for _, raw := range m.rawLeveeTransitions {
		var category string
		toEffective := effectiveState(raw.ToState)

		// Only categorize when effective state changes (CLOSED <-> OPEN)
		// This squashes consecutive OPEN/HALF_OPEN events
		if toEffective == lastEffectiveState {
			continue
		}

		if toEffective == levee.OPEN {
			// Circuit is opening (from CLOSED/INIT to OPEN/HALF_OPEN)
			// Check flapping first: <1m since last close
			if !lastLeveeCloseTime.IsZero() && raw.Timestamp.Sub(lastLeveeCloseTime) < flappingWindow {
				category = "flapping"
				m.Flapping++
			} else if raw.PrescientState == levee.CLOSED {
				// Prescient is currently closed, check if it stays closed within ±5s window
				if m.isPrescientClosedInWindow(raw.Timestamp, falseAlarmWindow) {
					category = "false_alarm"
					m.FalseAlarms++
				}
			} else if raw.PrescientState == levee.OPEN {
				// Prescient is open, check how long ago it opened
				prescientOpenTime := m.findLastPrescientOpenBefore(raw.Timestamp)
				if !prescientOpenTime.IsZero() && raw.Timestamp.Sub(prescientOpenTime) > lateDetectionDelay {
					category = "late_detection"
					m.LateDetections++
				}
			}
		} else if toEffective == levee.CLOSED {
			// Circuit is closing (from OPEN/HALF_OPEN to CLOSED)
			lastLeveeCloseTime = raw.Timestamp

			if raw.PrescientState == levee.OPEN {
				category = "premature_recovery"
				m.PrematureRecoveries++
			} else if raw.PrescientState == levee.CLOSED {
				// Check how long ago prescient closed
				prescientCloseTime := m.findLastPrescientCloseBefore(raw.Timestamp)
				if !prescientCloseTime.IsZero() && raw.Timestamp.Sub(prescientCloseTime) > slowRecoveryDelay {
					category = "slow_recovery"
					m.SlowRecoveries++
				}
			}
		}

		lastEffectiveState = toEffective

		if category != "" {
			m.LeveeStateTransitions = append(m.LeveeStateTransitions, StateTransitionWithPrescient{
				Timestamp:      raw.Timestamp,
				LeveeState:     raw.ToState,
				PrescientState: raw.PrescientState,
				Trigger:        raw.Trigger,
				Category:       category,
			})
		}
	}
}

// isPrescientClosedInWindow checks if prescient stays CLOSED within ±window of timestamp
func (m *PrescientMetrics) isPrescientClosedInWindow(t time.Time, window time.Duration) bool {
	windowStart := t.Add(-window)
	windowEnd := t.Add(window)

	// Check if any prescient OPEN period overlaps with [windowStart, windowEnd]
	var lastChangeTime time.Time

	for _, sc := range m.prescientStateChanges {
		if sc.Timestamp.After(windowEnd) {
			break
		}

		if sc.ToState == levee.OPEN {
			// Prescient opened - check if this overlaps with our window
			// The OPEN period starts at sc.Timestamp
			if sc.Timestamp.Before(windowEnd) {
				// Find when it closed
				closeTime := m.findNextPrescientCloseAfter(sc.Timestamp)
				if closeTime.IsZero() || closeTime.After(windowStart) {
					// OPEN period overlaps with window
					return false
				}
			}
		}

		lastChangeTime = sc.Timestamp
	}

	// If we never saw a state change before windowStart, check initial state
	if lastChangeTime.IsZero() || lastChangeTime.Before(windowStart) {
		// Check what state prescient was in at windowStart
		stateAtWindowStart := m.getPrescientStateAt(windowStart)
		if stateAtWindowStart == levee.OPEN {
			return false
		}
	}

	return true
}

// findLastPrescientOpenBefore finds when prescient last transitioned to OPEN before time t
func (m *PrescientMetrics) findLastPrescientOpenBefore(t time.Time) time.Time {
	var lastOpen time.Time
	for _, sc := range m.prescientStateChanges {
		if sc.Timestamp.After(t) {
			break
		}
		if sc.ToState == levee.OPEN {
			lastOpen = sc.Timestamp
		}
	}
	return lastOpen
}

// findLastPrescientCloseBefore finds when prescient last transitioned to CLOSED before time t
func (m *PrescientMetrics) findLastPrescientCloseBefore(t time.Time) time.Time {
	var lastClose time.Time
	for _, sc := range m.prescientStateChanges {
		if sc.Timestamp.After(t) {
			break
		}
		if sc.ToState == levee.CLOSED {
			lastClose = sc.Timestamp
		}
	}
	return lastClose
}

// findNextPrescientCloseAfter finds when prescient next transitions to CLOSED after time t
func (m *PrescientMetrics) findNextPrescientCloseAfter(t time.Time) time.Time {
	for _, sc := range m.prescientStateChanges {
		if sc.Timestamp.After(t) && sc.ToState == levee.CLOSED {
			return sc.Timestamp
		}
	}
	return time.Time{}
}

// getPrescientStateAt returns prescient state at a given time
func (m *PrescientMetrics) getPrescientStateAt(t time.Time) levee.State {
	state := levee.CLOSED
	for _, sc := range m.prescientStateChanges {
		if sc.Timestamp.After(t) {
			break
		}
		state = sc.ToState
	}
	return state
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

	// Categorize all transitions based on time windows
	m.categorizeTransitions()
}

// CandidateResult holds benchmark results for a single candidate
type CandidateResult struct {
	Name    string
	Metrics *PrescientMetrics
}

// ===============================================================
// Unified Multi-CB Analytics Structures
// ===============================================================

// CBRawTransition captures a state change for any circuit breaker
type CBRawTransition struct {
	CBName         string
	Timestamp      time.Time
	FromState      levee.State
	ToState        levee.State
	Trigger        levee.Trigger
	PrescientState levee.State
}

// CBClassifiedTransition represents a squashed and classified transition
type CBClassifiedTransition struct {
	CBName         string
	Timestamp      time.Time
	EffectiveState levee.State // OPEN or CLOSED (HALF_OPEN squashed to OPEN)
	PrescientState levee.State
	Classification string // Human-readable: "Late detection", "False alarm", etc.
	Trigger        levee.Trigger
}

// Incident represents a prescient OPEN period with per-CB metrics
type Incident struct {
	Start time.Time
	End   time.Time

	// Per-CB metrics during this incident
	CBMetrics map[string]*IncidentCBMetrics
}

// IncidentCBMetrics tracks a CB's behavior during an incident
type IncidentCBMetrics struct {
	RequestsAllowed int64
	MeanRPS         float64
	MaxRPS          float64
	Penalty         float64 // Sum of RPS² in 1-second windows
}

// UnifiedBenchmarkResult aggregates results from all CBs
type UnifiedBenchmarkResult struct {
	StartTime time.Time
	EndTime   time.Time
	SLO       levee.SLO

	// All CB names in order
	CBNames []string

	// Raw transitions from all CBs (for raw log)
	AllRawTransitions []CBRawTransition

	// Prescient state changes (shared across all CBs)
	PrescientChanges []PrescientStateChange

	// Classified transitions per CB
	ClassifiedTransitions map[string][]CBClassifiedTransition

	// Incidents (prescient OPEN periods) with per-CB metrics
	Incidents []Incident

	// Per-CB summary metrics
	PerCBMetrics map[string]*CBSummaryMetrics

	// Total penalty per CB
	TotalPenalty map[string]float64
}

// CBSummaryMetrics holds summary stats for a single CB
type CBSummaryMetrics struct {
	TotalAllowed        int64
	TotalBlocked        int64
	FalseAlarms         int64
	LateDetections      int64
	SlowRecoveries      int64
	Flapping            int64
	PrematureRecoveries int64
	ExtraBadAllowed     int64 // Allowed when prescient blocked
	ExtraGoodBlocked    int64 // Blocked when prescient allowed

	// RPS²-weighted penalties
	BadTrafficPenalty   float64 // Damage from letting bad traffic through
	LostBusinessPenalty float64 // Damage from blocking good traffic
}

// Classification labels (human-readable)
const (
	ClassFalseAlarm        = "False alarm"
	ClassLateDetection     = "Late detection"
	ClassSlowRecovery      = "Slow recovery"
	ClassFlapping          = "Flapping"
	ClassPrematureRecovery = "Premature recovery"
	ClassGoodOpen          = "Good detection"
	ClassGoodRecovery      = "Good recovery"
)

// ===============================================================
// Unified Analysis Functions
// ===============================================================

// buildUnifiedResult aggregates results from all CBs into a single unified result
func buildUnifiedResult(results []CandidateResult, slo levee.SLO) *UnifiedBenchmarkResult {
	if len(results) == 0 {
		return nil
	}

	unified := &UnifiedBenchmarkResult{
		StartTime:             results[0].Metrics.StartTime,
		EndTime:               results[0].Metrics.EndTime,
		SLO:                   slo,
		CBNames:               make([]string, 0, len(results)),
		AllRawTransitions:     make([]CBRawTransition, 0),
		PrescientChanges:      results[0].Metrics.prescientStateChanges, // Same for all
		ClassifiedTransitions: make(map[string][]CBClassifiedTransition),
		PerCBMetrics:          make(map[string]*CBSummaryMetrics),
		TotalPenalty:          make(map[string]float64),
	}

	// Collect raw transitions from all CBs
	for _, r := range results {
		unified.CBNames = append(unified.CBNames, r.Name)

		// Convert raw transitions to unified format
		for _, t := range r.Metrics.rawLeveeTransitions {
			unified.AllRawTransitions = append(unified.AllRawTransitions, CBRawTransition{
				CBName:         r.Name,
				Timestamp:      t.Timestamp,
				FromState:      t.FromState,
				ToState:        t.ToState,
				Trigger:        t.Trigger,
				PrescientState: t.PrescientState,
			})
		}

		// Copy summary metrics
		unified.PerCBMetrics[r.Name] = &CBSummaryMetrics{
			TotalAllowed:        r.Metrics.LeveeAllowed,
			TotalBlocked:        r.Metrics.LeveeBlocked,
			ExtraBadAllowed:     r.Metrics.ExtraBadRequestsAllowed,
			ExtraGoodBlocked:    r.Metrics.ExtraGoodRequestsBlocked,
			BadTrafficPenalty:   r.Metrics.BadTrafficPenalty,
			LostBusinessPenalty: r.Metrics.LostBusinessPenalty,
		}
	}

	// Sort all raw transitions by timestamp
	sortRawTransitions(unified.AllRawTransitions)

	// Classify transitions for each CB
	for _, r := range results {
		unified.ClassifiedTransitions[r.Name] = classifyTransitionsForCB(
			r.Name, r.Metrics.rawLeveeTransitions, unified.PrescientChanges, unified.StartTime)

		// Update classification counts
		for _, ct := range unified.ClassifiedTransitions[r.Name] {
			metrics := unified.PerCBMetrics[r.Name]
			switch ct.Classification {
			case ClassFalseAlarm:
				metrics.FalseAlarms++
			case ClassLateDetection:
				metrics.LateDetections++
			case ClassSlowRecovery:
				metrics.SlowRecoveries++
			case ClassFlapping:
				metrics.Flapping++
			case ClassPrematureRecovery:
				metrics.PrematureRecoveries++
			}
		}
	}

	// Identify incidents and compute metrics
	unified.Incidents = identifyIncidents(unified.PrescientChanges, unified.StartTime, unified.EndTime)
	computeIncidentMetrics(unified, results)

	// Compute total penalties
	for _, name := range unified.CBNames {
		var total float64
		for _, inc := range unified.Incidents {
			if m, ok := inc.CBMetrics[name]; ok {
				total += m.Penalty
			}
		}
		unified.TotalPenalty[name] = total
	}

	return unified
}

// sortRawTransitions sorts transitions by timestamp
func sortRawTransitions(transitions []CBRawTransition) {
	// Simple bubble sort (sufficient for our use case)
	for i := range transitions {
		for j := i + 1; j < len(transitions); j++ {
			if transitions[j].Timestamp.Before(transitions[i].Timestamp) {
				transitions[i], transitions[j] = transitions[j], transitions[i]
			}
		}
	}
}

// classifyTransitionsForCB classifies transitions for a single CB with human-readable labels
// ALL effective state changes are recorded, not just "bad" ones
func classifyTransitionsForCB(cbName string, raw []RawLeveeTransition, prescientChanges []PrescientStateChange, startTime time.Time) []CBClassifiedTransition {
	const (
		falseAlarmWindow   = 5 * time.Second
		lateDetectionDelay = 1 * time.Second
		slowRecoveryDelay  = 5 * time.Second
		flappingWindow     = 1 * time.Minute
	)

	var result []CBClassifiedTransition
	var lastCloseTime time.Time
	var lastEffective levee.State = levee.CLOSED

	for _, t := range raw {
		toEffective := effectiveState(t.ToState)

		// Only classify when effective state changes
		if toEffective == lastEffective {
			continue
		}

		var classification string

		if toEffective == levee.OPEN {
			// Circuit opening
			if !lastCloseTime.IsZero() && t.Timestamp.Sub(lastCloseTime) < flappingWindow {
				classification = ClassFlapping
			} else if t.PrescientState == levee.CLOSED {
				if isPrescientClosedInWindowForCB(prescientChanges, t.Timestamp, falseAlarmWindow, startTime) {
					classification = ClassFalseAlarm
				}
			} else if t.PrescientState == levee.OPEN {
				openTime := findLastPrescientOpenBeforeForCB(prescientChanges, t.Timestamp)
				if !openTime.IsZero() && t.Timestamp.Sub(openTime) > lateDetectionDelay {
					classification = ClassLateDetection
				} else {
					// Opened within 1s of prescient - this is GOOD behavior!
					classification = ClassGoodOpen
				}
			}
		} else if toEffective == levee.CLOSED {
			// Circuit closing
			lastCloseTime = t.Timestamp

			if t.PrescientState == levee.OPEN {
				classification = ClassPrematureRecovery
			} else if t.PrescientState == levee.CLOSED {
				closeTime := findLastPrescientCloseBeforeForCB(prescientChanges, t.Timestamp)
				if !closeTime.IsZero() && t.Timestamp.Sub(closeTime) > slowRecoveryDelay {
					classification = ClassSlowRecovery
				} else {
					// Closed within 5s of prescient - this is GOOD behavior!
					classification = ClassGoodRecovery
				}
			}
		}

		lastEffective = toEffective

		// Record ALL effective state changes, not just "bad" ones
		if classification != "" {
			result = append(result, CBClassifiedTransition{
				CBName:         cbName,
				Timestamp:      t.Timestamp,
				EffectiveState: toEffective,
				PrescientState: t.PrescientState,
				Classification: classification,
				Trigger:        t.Trigger,
			})
		}
	}

	return result
}

// Helper functions for classification
func isPrescientClosedInWindowForCB(changes []PrescientStateChange, t time.Time, window time.Duration, startTime time.Time) bool {
	windowStart := t.Add(-window)
	windowEnd := t.Add(window)

	for _, sc := range changes {
		if sc.Timestamp.After(windowEnd) {
			break
		}
		if sc.ToState == levee.OPEN {
			closeTime := findNextPrescientCloseAfterForCB(changes, sc.Timestamp)
			if closeTime.IsZero() || closeTime.After(windowStart) {
				if sc.Timestamp.Before(windowEnd) {
					return false
				}
			}
		}
	}
	return true
}

func findLastPrescientOpenBeforeForCB(changes []PrescientStateChange, t time.Time) time.Time {
	var last time.Time
	for _, sc := range changes {
		if sc.Timestamp.After(t) {
			break
		}
		if sc.ToState == levee.OPEN {
			last = sc.Timestamp
		}
	}
	return last
}

func findLastPrescientCloseBeforeForCB(changes []PrescientStateChange, t time.Time) time.Time {
	var last time.Time
	for _, sc := range changes {
		if sc.Timestamp.After(t) {
			break
		}
		if sc.ToState == levee.CLOSED {
			last = sc.Timestamp
		}
	}
	return last
}

func findNextPrescientCloseAfterForCB(changes []PrescientStateChange, t time.Time) time.Time {
	for _, sc := range changes {
		if sc.Timestamp.After(t) && sc.ToState == levee.CLOSED {
			return sc.Timestamp
		}
	}
	return time.Time{}
}

// identifyIncidents finds all prescient OPEN periods
func identifyIncidents(changes []PrescientStateChange, startTime, endTime time.Time) []Incident {
	var incidents []Incident
	var currentStart time.Time
	inIncident := false

	for _, sc := range changes {
		if sc.ToState == levee.OPEN && !inIncident {
			currentStart = sc.Timestamp
			inIncident = true
		} else if sc.ToState == levee.CLOSED && inIncident {
			incidents = append(incidents, Incident{
				Start:     currentStart,
				End:       sc.Timestamp,
				CBMetrics: make(map[string]*IncidentCBMetrics),
			})
			inIncident = false
		}
	}

	// Handle case where incident extends to end of benchmark
	if inIncident {
		incidents = append(incidents, Incident{
			Start:     currentStart,
			End:       endTime,
			CBMetrics: make(map[string]*IncidentCBMetrics),
		})
	}

	return incidents
}

// computeIncidentMetrics calculates per-CB RPS and penalty for each incident
func computeIncidentMetrics(unified *UnifiedBenchmarkResult, results []CandidateResult) {
	for i := range unified.Incidents {
		inc := &unified.Incidents[i]

		for _, r := range results {
			incDuration := inc.End.Sub(inc.Start)
			incSeconds := int64(incDuration.Seconds())
			if incSeconds == 0 {
				incSeconds = 1
			}

			// Calculate OPEN duration properly by tracking effective state changes
			// Only count transitions from effective-CLOSED to effective-OPEN
			var openDuration time.Duration
			var closedDuration time.Duration

			// First, determine the CB's effective state at incident start
			prevEffective := levee.CLOSED // Default assumption
			for _, t := range r.Metrics.rawLeveeTransitions {
				if t.Timestamp.After(inc.Start) {
					break
				}
				prevEffective = effectiveState(t.ToState)
			}

			// If CB was already OPEN at incident start, count from start
			var openStartTime time.Time
			if prevEffective == levee.OPEN {
				openStartTime = inc.Start
			}

			// Track state changes during the incident
			for _, t := range r.Metrics.rawLeveeTransitions {
				if t.Timestamp.Before(inc.Start) {
					continue
				}
				if t.Timestamp.After(inc.End) {
					break
				}

				currEffective := effectiveState(t.ToState)
				if currEffective == prevEffective {
					continue // No effective state change
				}

				if currEffective == levee.OPEN && prevEffective != levee.OPEN {
					// Transition to OPEN - start counting
					openStartTime = t.Timestamp
				} else if currEffective == levee.CLOSED && prevEffective == levee.OPEN {
					// Transition to CLOSED - end counting
					if !openStartTime.IsZero() {
						openDuration += t.Timestamp.Sub(openStartTime)
					}
					openStartTime = time.Time{}
				}
				prevEffective = currEffective
			}

			// If still OPEN at end of incident, count until end
			if !openStartTime.IsZero() {
				openDuration += inc.End.Sub(openStartTime)
			}

			// CLOSED duration is the complement
			closedDuration = incDuration - openDuration

			// Estimate allowed requests during incident based on throttling behavior
			var reqsAllowed int64
			var meanRPS, maxRPS, penalty float64

			// Estimate RPS based on total allowed requests across entire benchmark
			totalSeconds := unified.EndTime.Sub(unified.StartTime).Seconds()
			baseRPS := 0.0
			if totalSeconds > 0 {
				baseRPS = float64(r.Metrics.LeveeAllowed) / totalSeconds
			}

			// During OPEN: only probing traffic (low RPS)
			// During CLOSED: full traffic (high RPS) - THIS IS BAD during an incident!
			probeRPS := 5.0 // Typical probe rate during OPEN/HALF_OPEN

			// Requests allowed = probing during OPEN + full traffic during CLOSED
			reqsFromOpen := probeRPS * openDuration.Seconds()
			reqsFromClosed := baseRPS * closedDuration.Seconds()
			reqsAllowed = int64(reqsFromOpen + reqsFromClosed)

			// Penalty: RPS² during CLOSED periods (when CB should have been blocking)
			// OPEN periods have minimal penalty (just probe traffic)
			penaltyFromClosed := baseRPS * baseRPS * closedDuration.Seconds()
			penaltyFromOpen := probeRPS * probeRPS * openDuration.Seconds()
			penalty = penaltyFromClosed + penaltyFromOpen

			// Mean RPS
			if incDuration.Seconds() > 0 {
				meanRPS = float64(reqsAllowed) / incDuration.Seconds()
			}
			maxRPS = baseRPS // Worst case is full traffic during CLOSED periods

			inc.CBMetrics[r.Name] = &IncidentCBMetrics{
				RequestsAllowed: reqsAllowed,
				MeanRPS:         meanRPS,
				MaxRPS:          maxRPS,
				Penalty:         penalty,
			}
		}
	}
}

// ===============================================================
// Unified Reporting Functions
// ===============================================================

// formatElapsed formats a duration as "XXhYYmZZ.ZZs" with consistent width
func formatElapsed(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := d.Seconds() - float64(hours*3600+minutes*60)
	return fmt.Sprintf("%2dh%02dm%05.2fs", hours, minutes, seconds)
}

// writeUnifiedRawLog writes all raw transitions from all CBs to a file
func writeUnifiedRawLog(unified *UnifiedBenchmarkResult) {
	f, err := os.Create("transition_log_raw.txt")
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Raw Transition Log (All Circuit Breakers) ===\n")
	fmt.Fprintf(f, "Total transitions: %d\n", len(unified.AllRawTransitions))
	fmt.Fprintf(f, "Circuit breakers: %s\n", strings.Join(unified.CBNames, ", "))
	fmt.Fprintf(f, "Start Time: %v\n", unified.StartTime)
	fmt.Fprintf(f, "End Time: %v\n", unified.EndTime)
	fmt.Fprintf(f, "Total Duration: %v\n\n", unified.EndTime.Sub(unified.StartTime))

	for i, t := range unified.AllRawTransitions {
		elapsed := t.Timestamp.Sub(unified.StartTime)
		triggerStr := "<none>"
		if t.Trigger != nil {
			triggerStr = t.Trigger.Error()
		}
		fmt.Fprintf(f, "[%4d] %12v | %-12s | %s -> %s | Prescient=%s | %s\n",
			i+1, elapsed, t.CBName, stateString(t.FromState), stateString(t.ToState),
			stateString(t.PrescientState), triggerStr)
	}
}

// writeUnifiedClassifiedLog writes interleaved classified transitions with incident summaries
func writeUnifiedClassifiedLog(unified *UnifiedBenchmarkResult) {
	f, err := os.Create("transition_log_classified.txt")
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Classified Transition Log (All Circuit Breakers) ===\n")
	fmt.Fprintf(f, "Circuit breakers: %s\n", strings.Join(unified.CBNames, ", "))
	fmt.Fprintf(f, "Start Time: %v\n", unified.StartTime)
	fmt.Fprintf(f, "End Time: %v\n", unified.EndTime)
	fmt.Fprintf(f, "Total Duration: %v\n\n", unified.EndTime.Sub(unified.StartTime))

	// Collect all classified transitions and sort by timestamp
	type transitionWithCB struct {
		transition CBClassifiedTransition
		cbName     string
	}
	var allTransitions []transitionWithCB
	for cbName, transitions := range unified.ClassifiedTransitions {
		for _, t := range transitions {
			allTransitions = append(allTransitions, transitionWithCB{t, cbName})
		}
	}

	// Sort by timestamp
	for i := range allTransitions {
		for j := i + 1; j < len(allTransitions); j++ {
			if allTransitions[j].transition.Timestamp.Before(allTransitions[i].transition.Timestamp) {
				allTransitions[i], allTransitions[j] = allTransitions[j], allTransitions[i]
			}
		}
	}

	// Column widths
	const (
		tsWidth    = 18
		stateWidth = 12
	)

	// Track current state for each CB
	cbStates := make(map[string]levee.State)
	for _, name := range unified.CBNames {
		cbStates[name] = levee.CLOSED
	}
	prescientState := levee.CLOSED

	// Build header
	header := fmt.Sprintf("%-*s | %-*s", tsWidth, "Timestamp", stateWidth, "Prescient")
	for _, name := range unified.CBNames {
		header += fmt.Sprintf(" | %-*s", stateWidth, name)
	}
	header += " | Classification"
	fmt.Fprintf(f, "%s\n", header)
	fmt.Fprintf(f, "%s\n", strings.Repeat("-", len(header)))

	// Track which incident we're in
	incidentIdx := 0

	for _, tw := range allTransitions {
		t := tw.transition
		elapsed := t.Timestamp.Sub(unified.StartTime)

		// Format elapsed time to fixed width (e.g., "4h06m53.58s")
		elapsedStr := formatElapsed(elapsed)

		// Update prescient state
		for _, pc := range unified.PrescientChanges {
			if pc.Timestamp.Before(t.Timestamp) || pc.Timestamp.Equal(t.Timestamp) {
				prescientState = pc.ToState
			}
		}

		// Check if we need to print an incident summary (when transitioning out of an incident)
		for incidentIdx < len(unified.Incidents) {
			inc := unified.Incidents[incidentIdx]
			if t.Timestamp.After(inc.End) {
				// Print incident summary
				fmt.Fprintf(f, "\n--- Incident Summary: %s to %s (Prescient OPEN) ---\n",
					formatElapsed(inc.Start.Sub(unified.StartTime)),
					formatElapsed(inc.End.Sub(unified.StartTime)))
				for _, cbName := range unified.CBNames {
					if m, ok := inc.CBMetrics[cbName]; ok {
						fmt.Fprintf(f, "%-12s: Reqs allowed: %6d  Mean RPS: %5.0f  Max RPS: %5.0f  Penalty: %9.0f\n",
							cbName, m.RequestsAllowed, m.MeanRPS, m.MaxRPS, m.Penalty)
					}
				}
				fmt.Fprintf(f, "\n")
				incidentIdx++
			} else {
				break
			}
		}

		// Build state columns
		row := fmt.Sprintf("%-*s | %-*s", tsWidth, elapsedStr, stateWidth, stateString(prescientState))
		for _, name := range unified.CBNames {
			var stateStr string
			if name == t.CBName {
				// This CB changed state
				cbStates[name] = t.EffectiveState
				stateStr = stateString(t.EffectiveState)
			} else {
				stateStr = "..." + stateString(cbStates[name])
			}
			row += fmt.Sprintf(" | %-*s", stateWidth, stateStr)
		}
		row += fmt.Sprintf(" | %s", t.Classification)
		fmt.Fprintf(f, "%s\n", row)

		// Print trigger on next line if available
		if t.Trigger != nil {
			triggerRow := fmt.Sprintf("%-*s | %-*s", tsWidth, "", stateWidth, "")
			for _, name := range unified.CBNames {
				if name == t.CBName {
					triggerStr := t.Trigger.Error()
					if len(triggerStr) > stateWidth {
						triggerStr = triggerStr[:stateWidth]
					}
					triggerRow += fmt.Sprintf(" | %-*s", stateWidth, triggerStr)
				} else {
					triggerRow += fmt.Sprintf(" | %-*s", stateWidth, "")
				}
			}
			fmt.Fprintf(f, "%s\n", triggerRow)
		}
	}

	// Print any remaining incident summaries
	for ; incidentIdx < len(unified.Incidents); incidentIdx++ {
		inc := unified.Incidents[incidentIdx]
		fmt.Fprintf(f, "\n--- Incident Summary: %s to %s (Prescient OPEN) ---\n",
			formatElapsed(inc.Start.Sub(unified.StartTime)),
			formatElapsed(inc.End.Sub(unified.StartTime)))
		for _, cbName := range unified.CBNames {
			if m, ok := inc.CBMetrics[cbName]; ok {
				fmt.Fprintf(f, "%-12s: Reqs allowed: %6d  Mean RPS: %5.0f  Max RPS: %5.0f  Penalty: %9.0f\n",
					cbName, m.RequestsAllowed, m.MeanRPS, m.MaxRPS, m.Penalty)
			}
		}
	}

	// Print total summary
	fmt.Fprintf(f, "\n=== Total Penalty Summary ===\n")
	for _, cbName := range unified.CBNames {
		fmt.Fprintf(f, "%-12s: %9.0f\n", cbName, unified.TotalPenalty[cbName])
	}
}

// writeUnifiedComparativeSummary writes the comparative summary to stderr
func writeUnifiedComparativeSummary(unified *UnifiedBenchmarkResult) {
	sep := strings.Repeat("=", 130)
	fmt.Fprintf(os.Stderr, "\n%s\n", sep)
	fmt.Fprintf(os.Stderr, "  COMPARATIVE SUMMARY - All Candidates vs Prescient (Ideal)\n")
	fmt.Fprintf(os.Stderr, "%s\n\n", sep)

	// Header
	fmt.Fprintf(os.Stderr, "%-15s | %10s | %10s | %4s | %10s | %10s | %12s | %12s | %12s\n",
		"Candidate", "Blocked", "Allowed", "Flap", "FalseAlarm", "LateDetect", "BadTraffic", "LostBusiness", "TotalPenalty")
	fmt.Fprintf(os.Stderr, "%s\n", strings.Repeat("-", 130))

	// Prescient row (ideal)
	prescientBlocked := int64(0)
	prescientAllowed := int64(0)
	if len(unified.CBNames) > 0 {
		// Get from first CB's metrics (prescient is same for all)
		if m, ok := unified.PerCBMetrics[unified.CBNames[0]]; ok {
			totalReqs := m.TotalAllowed + m.TotalBlocked
			// Prescient would have blocked during OPEN periods
			for _, inc := range unified.Incidents {
				incDuration := inc.End.Sub(inc.Start).Seconds()
				totalDuration := unified.EndTime.Sub(unified.StartTime).Seconds()
				if totalDuration > 0 {
					prescientBlocked += int64(float64(totalReqs) * incDuration / totalDuration)
				}
			}
			prescientAllowed = totalReqs - prescientBlocked
		}
	}
	fmt.Fprintf(os.Stderr, "%-15s | %10d | %10d | %4d | %10d | %10d | %12d | %12d | %12d\n",
		"Prescient", prescientBlocked, prescientAllowed, 0, 0, 0, 0, 0, 0)

	// CB rows sorted by ascending TotalPenalty
	sortedNames := make([]string, len(unified.CBNames))
	copy(sortedNames, unified.CBNames)
	sort.Slice(sortedNames, func(i, j int) bool {
		mi := unified.PerCBMetrics[sortedNames[i]]
		mj := unified.PerCBMetrics[sortedNames[j]]
		penaltyI := math.Sqrt(mi.BadTrafficPenalty) + math.Sqrt(mi.LostBusinessPenalty)
		penaltyJ := math.Sqrt(mj.BadTrafficPenalty) + math.Sqrt(mj.LostBusinessPenalty)
		return penaltyI < penaltyJ
	})

	for _, name := range sortedNames {
		m := unified.PerCBMetrics[name]
		fmt.Fprintf(os.Stderr, "%-15s | %10d | %10d | %4d | %10d | %10d | %12.0f | %12.0f | %12.0f\n",
			name, m.TotalBlocked, m.TotalAllowed, m.Flapping, m.FalseAlarms,
			m.LateDetections, math.Sqrt(m.BadTrafficPenalty), math.Sqrt(m.LostBusinessPenalty),
			math.Sqrt(m.BadTrafficPenalty)+math.Sqrt(m.LostBusinessPenalty))
	}

	fmt.Fprintf(os.Stderr, "\nBadTraffic = √Σ(RPS²) when Prescient OPEN but CB allowed (lower = better)\n")
	fmt.Fprintf(os.Stderr, "LostBusiness = √Σ(RPS²) when Prescient CLOSED but CB blocked (lower = better)\n")
	fmt.Fprintf(os.Stderr, "%s\n", sep)
}

// runBenchmark runs the benchmark for a single ICircuitBreaker candidate with optional time windowing
func runBenchmark(slo levee.SLO, specs []loadgen.LoadSpec, breaker CircuitBreaker, cfg BenchmarkConfig) *PrescientMetrics {
	metrics := NewPrescientMetrics()
	penalty := &RunningPenalty{}

	gen := loadgen.NewLoadGenerator(specs)
	stream := loadgen.NewEventStream(gen)

	var prescient *PrescientBreaker
	var simulationStartTime time.Time

	pending := make(map[int64]pendingRequest)

	breakerHasOpened := false
	var lastEvent loadgen.SimEvent
	var prevBreakerState levee.State = breaker.State()
	var prevPrescientState levee.State = levee.CLOSED

	// Skip to start offset if specified
	if cfg.StartOffset > 0 {
		for {
			event, err := stream.Next()
			if err != nil {
				return metrics // Reached end before start offset
			}

			if simulationStartTime.IsZero() {
				simulationStartTime = event.Timestamp
				prescient = NewPrescientBreaker(slo, specs, event.Timestamp)
				metrics.StartTime = event.Timestamp
			}

			elapsed := event.Timestamp.Sub(simulationStartTime)
			if elapsed >= cfg.StartOffset {
				lastEvent = event
				break
			}
		}
	}

	// Process events
	for {
		event, err := stream.Next()
		if err != nil {
			break
		}
		lastEvent = event

		if simulationStartTime.IsZero() {
			simulationStartTime = event.Timestamp
			prescient = NewPrescientBreaker(slo, specs, event.Timestamp)
			metrics.StartTime = event.Timestamp
		}

		// Stop at end offset if specified
		if cfg.EndOffset > 0 {
			elapsed := event.Timestamp.Sub(simulationStartTime)
			if elapsed >= cfg.EndOffset {
				break
			}
		}

		switch event.Status {
		case loadgen.EventStart:
			breakerStateChange, breakerErr := breaker.Start(event.Timestamp)
			breakerState := breakerStateChange.State
			prescientState := prescient.State(event.Timestamp)

			prevPrescientState = recordPrescientStateChange(metrics, prevPrescientState, prescientState, event.Timestamp)
			prevBreakerState = recordBreakerStateChange(metrics, breakerStateChange, prevBreakerState, prescientState, event.Timestamp)

			// Track first state changes for lag calculations
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

			// Track RPS²-based penalties on-the-fly
			prescientOpen := (prescientState == levee.OPEN)
			penalty.Record(event.Timestamp, prescientOpen, breakerAllowed)

			pending[event.EventID] = pendingRequest{
				startTime:        event.Timestamp,
				breakerAllowed:   breakerAllowed,
				prescientAllowed: prescientAllowed,
			}

		case loadgen.EventSuccess:
			req := pending[event.EventID]
			duration := event.Timestamp.Sub(req.startTime)

			metrics.TotalRequests++
			metrics.TotalSuccesses++

			if req.breakerAllowed {
				sc := breaker.Success(event.Timestamp, duration)
				prevBreakerState = recordBreakerStateChange(metrics, sc, prevBreakerState, prescient.State(event.Timestamp), event.Timestamp)
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
				sc := breaker.Fail(event.Timestamp, duration)
				prevBreakerState = recordBreakerStateChange(metrics, sc, prevBreakerState, prescient.State(event.Timestamp), event.Timestamp)
			}

			if !req.prescientAllowed && req.breakerAllowed {
				metrics.ExtraBadRequestsAllowed++
			}

			delete(pending, event.EventID)
		}
	}

	// Finalize penalty tracking and copy to metrics
	penalty.Finalize()
	metrics.BadTrafficPenalty = penalty.BadTrafficPenalty
	metrics.LostBusinessPenalty = penalty.LostBusinessPenalty

	metrics.Finalize(lastEvent.Timestamp)
	return metrics
}

// BenchmarkCyberMondayPrescient runs benchmark against prescient breaker for all candidates
func BenchmarkCyberMondayPrescient(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
	}

	specs := generateCyberMondayWorkload()

	fmt.Fprintf(os.Stderr, "Generated %d load specifications for Cyber Monday simulation\n", len(specs))

	// Run benchmark for each candidate in parallel
	candidates := []struct {
		name    string
		breaker CircuitBreaker
	}{
		{"Levee", levee.NewLevee(slo)},
		{"Static-BAU", NewStaticBAU()},
		{"Static-Peak", NewStaticPeak()},
	}

	var wg sync.WaitGroup
	resultsChan := make(chan CandidateResult, len(candidates))

	for _, c := range candidates {
		wg.Add(1)
		go func(name string, breaker CircuitBreaker) {
			defer wg.Done()
			fmt.Fprintf(os.Stderr, "\n>>> Running benchmark for %s...\n", name)
			metrics := runCandidateBenchmark(slo, specs, breaker)
			resultsChan <- CandidateResult{Name: name, Metrics: metrics}
		}(c.name, c.breaker)
	}

	wg.Wait()
	close(resultsChan)

	results := make([]CandidateResult, 0, len(candidates))
	for r := range resultsChan {
		results = append(results, r)
	}

	// Build unified result and generate reports
	unified := buildUnifiedResult(results, slo)
	if unified != nil {
		writeUnifiedRawLog(unified)
		writeUnifiedClassifiedLog(unified)
		writeUnifiedComparativeSummary(unified)
	}
}

// runCandidateBenchmark runs the benchmark for a single ICircuitBreaker candidate
func runCandidateBenchmark(slo levee.SLO, specs []loadgen.LoadSpec, breaker CircuitBreaker) *PrescientMetrics {
	return runBenchmark(slo, specs, breaker, BenchmarkConfig{})
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
			if req, ok := pending[event.EventID]; ok {
				duration := event.Timestamp.Sub(req.startTime)
				lev.Success(event.Timestamp, duration)
				delete(pending, event.EventID)
			}
		case loadgen.EventError:
			if req, ok := pending[event.EventID]; ok {
				duration := event.Timestamp.Sub(req.startTime)
				lev.Fail(event.Timestamp, duration)
				delete(pending, event.EventID)
			}
		}
	}

	return lev
}

// BenchmarkGenerateStateFile builds Levee state up to h+03:59 and saves to file
func BenchmarkGenerateStateFile(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
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

// BenchmarkCyberMondayPrescientTruncated runs truncated benchmark from h+04:00 to h+04:20
func BenchmarkCyberMondayPrescientTruncated(b *testing.B) {
	slo := levee.SLO{
		SuccessRate: 0.90,
		Timeout:     1500 * time.Millisecond,
	}

	specs := generateCyberMondayWorkload()

	// Load saved state from file
	data, err := os.ReadFile("levee_state_h03_59.json")
	if err != nil {
		b.Fatalf("Failed to read state file: %v. Run BenchmarkGenerateStateFile first.", err)
	}

	var savedState levee.LeveeState
	err = json.Unmarshal(data, &savedState)
	if err != nil {
		b.Fatalf("Failed to unmarshal state: %v", err)
	}

	fmt.Fprintf(os.Stderr, "Loaded Levee state from file\n")

	// Run the truncated benchmark
	metrics := runTruncatedPrescientBenchmark(slo, specs, &savedState, 4*time.Hour, 4*time.Hour+20*time.Minute)

	// Build unified result and generate reports
	results := []CandidateResult{{Name: "Levee", Metrics: metrics}}
	unified := buildUnifiedResult(results, slo)
	if unified != nil {
		writeUnifiedRawLog(unified)
		writeUnifiedClassifiedLog(unified)
		writeUnifiedComparativeSummary(unified)
	}
}

// runTruncatedPrescientBenchmark runs benchmark in a specific time window using restored state
func runTruncatedPrescientBenchmark(slo levee.SLO, specs []loadgen.LoadSpec, savedState *levee.LeveeState, startOffset, endOffset time.Duration) *PrescientMetrics {
	breaker := levee.RestoreState(savedState)
	return runBenchmark(slo, specs, breaker, BenchmarkConfig{
		StartOffset: startOffset,
		EndOffset:   endOffset,
	})
}
