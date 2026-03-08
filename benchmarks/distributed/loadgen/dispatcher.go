// Package loadgen implements the load generator for the distributed benchmark.
package loadgen

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"time"

	"github.com/codemartial/levee/benchmarks/distributed/api"
	"github.com/codemartial/loadgen"
)

// DispatcherConfig holds configuration for the dispatcher.
type DispatcherConfig struct {
	HandleRequest func(api.AppRequest) api.AppResponse
	Specs         []loadgen.LoadSpec
	Seed          uint64
}

// Dispatcher generates load and sends requests to the app.
// Uses logical time for fast simulation - no wall clock sleeping.
// Requests are dispatched sequentially in logical-time order to ensure
// deterministic simulation results.
type Dispatcher struct {
	handleRequestFn func(api.AppRequest) api.AppResponse
	specs           []loadgen.LoadSpec
	rng             *rand.Rand

	// Current spec tracking (based on logical time)
	currentSpec int
	specStartNS int64 // Logical time when current spec started

	// Logical time tracking
	logicalTimeNS int64 // Current logical timestamp in nanoseconds

	// Cached inter-arrival time (nanoseconds)
	interArrivalTimeNS float64

	// Metrics
	totalRequests  int64
	totalResponses int64
	requestCounter int64

	// Wall time tracking (for throughput reporting)
	wallStartTime time.Time
}

// NewDispatcher creates a new load dispatcher.
func NewDispatcher(config DispatcherConfig) *Dispatcher {
	d := &Dispatcher{
		handleRequestFn: config.HandleRequest,
		specs:           config.Specs,
		rng:             rand.New(rand.NewPCG(config.Seed, config.Seed>>32)),
		logicalTimeNS:   0,
		specStartNS:     0,
	}

	if len(d.specs) > 0 {
		d.updateInterArrival(d.specs[0])
	}

	return d
}

// updateInterArrival updates the inter-arrival time for the current spec.
func (d *Dispatcher) updateInterArrival(spec loadgen.LoadSpec) {
	d.interArrivalTimeNS = (60.0 * 1e9) / float64(spec.RPM)
}

// Run starts the dispatcher and sends requests until all specs are exhausted.
// Uses logical time - completes 28 hours of simulation in minutes.
// Requests are processed sequentially in logical-time order for determinism.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.wallStartTime = time.Now()

	log.Printf("Dispatcher starting with logical time, %d specs, first spec RPM: %d",
		len(d.specs), d.specs[0].RPM)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Check if we've exhausted all specs
		if d.currentSpec >= len(d.specs) {
			log.Printf("All specs exhausted after %d requests (logical time: %.1f hours, wall time: %.1f seconds)",
				d.totalRequests,
				float64(d.logicalTimeNS)/float64(time.Hour),
				time.Since(d.wallStartTime).Seconds())
			return nil
		}

		// Calculate next logical timestamp using exponential inter-arrival
		interval := d.rng.ExpFloat64() * d.interArrivalTimeNS
		d.logicalTimeNS += int64(interval)

		// Check spec transition (based on logical time)
		d.maybeAdvanceSpec()

		// Check again after potential spec advance
		if d.currentSpec >= len(d.specs) {
			continue
		}

		// Create request with logical timestamp
		d.requestCounter++
		req := api.AppRequest{
			RequestID:   fmt.Sprintf("req-%d", d.requestCounter),
			TimestampNS: d.logicalTimeNS,
			SpecIndex:   d.currentSpec,
			TimeoutMS:   int(d.specs[d.currentSpec].TimeoutMS),
		}

		d.totalRequests++
		d.handleRequestFn(req)
		d.totalResponses++
	}
}

// maybeAdvanceSpec checks if we should move to the next spec based on logical time.
func (d *Dispatcher) maybeAdvanceSpec() {
	if d.currentSpec >= len(d.specs) {
		return
	}

	spec := d.specs[d.currentSpec]
	specDurationNS := int64(spec.DurationS) * int64(time.Second)

	if d.logicalTimeNS-d.specStartNS >= specDurationNS {
		d.currentSpec++
		d.specStartNS = d.logicalTimeNS

		if d.currentSpec < len(d.specs) {
			newSpec := d.specs[d.currentSpec]
			d.updateInterArrival(newSpec)
		}
	}
}

// GetStatus returns the current dispatcher status.
func (d *Dispatcher) GetStatus() api.LoadGenStatus {
	return api.LoadGenStatus{
		CurrentSpecIndex:  d.currentSpec,
		ElapsedSeconds:    time.Since(d.wallStartTime).Seconds(),
		LogicalTimeHours:  float64(d.logicalTimeNS) / float64(time.Hour),
		TotalRequests:     d.totalRequests,
		CurrentRPS:        d.calculateCurrentRPS(),
		TotalLogicalHours: d.calculateTotalLogicalHours(),
	}
}

// calculateCurrentRPS estimates current wall-clock RPS.
func (d *Dispatcher) calculateCurrentRPS() float64 {
	elapsed := time.Since(d.wallStartTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(d.totalRequests) / elapsed
}

// calculateTotalLogicalHours returns the total duration of all specs in hours.
func (d *Dispatcher) calculateTotalLogicalHours() float64 {
	var totalSeconds int64
	for _, spec := range d.specs {
		totalSeconds += int64(spec.DurationS)
	}
	return float64(totalSeconds) / 3600.0
}
