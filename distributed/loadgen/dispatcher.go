// Package loadgen implements the load generator for the distributed benchmark.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codemartial/levee/distributed/api"
	"github.com/codemartial/loadgen"
)

// DispatcherConfig holds configuration for the dispatcher.
type DispatcherConfig struct {
	AppHost string
	Specs   []loadgen.LoadSpec
	Seed    uint64
}

// Dispatcher generates load and sends requests to the app container.
// Uses logical time for fast simulation - no wall clock sleeping.
type Dispatcher struct {
	appHost    string
	specs      []loadgen.LoadSpec
	httpClient *http.Client
	rng        *rand.Rand

	// Current spec tracking (based on logical time)
	currentSpec int
	specStartNS int64 // Logical time when current spec started

	// Logical time tracking
	logicalTimeNS int64 // Current logical timestamp in nanoseconds

	// Cached inter-arrival time (nanoseconds)
	interArrivalTimeNS float64

	// Metrics
	totalRequests  atomic.Int64
	totalResponses atomic.Int64
	totalErrors    atomic.Int64
	requestCounter int64

	// Wall time tracking (for throughput reporting)
	wallStartTime time.Time
}

// NewDispatcher creates a new load dispatcher.
func NewDispatcher(config DispatcherConfig) *Dispatcher {
	// Create HTTP transport - use Unix socket if app host starts with "/"
	var transport *http.Transport
	if len(config.AppHost) > 0 && config.AppHost[0] == '/' {
		// Unix socket transport
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", config.AppHost)
			},
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		}
	} else {
		// TCP transport
		transport = &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		}
	}

	d := &Dispatcher{
		appHost: config.AppHost,
		specs:   config.Specs,
		rng:     rand.New(rand.NewPCG(config.Seed, config.Seed>>32)),
		httpClient: &http.Client{
			Timeout:   30 * time.Second, // Wall clock timeout for HTTP
			Transport: transport,
		},
		logicalTimeNS: 0,
		specStartNS:   0,
	}

	if len(d.specs) > 0 {
		d.updateInterArrival(d.specs[0])
	}

	return d
}

// updateInterArrival updates the inter-arrival time for the current spec.
func (d *Dispatcher) updateInterArrival(spec loadgen.LoadSpec) {
	// From loadgen.go: interArrivalTime = (60.0 * 1e9) / float64(spec.RPM)
	d.interArrivalTimeNS = (60.0 * 1e9) / float64(spec.RPM)
}

// Run starts the dispatcher and sends requests until all specs are exhausted.
// Uses logical time - completes 28 hours of simulation in minutes.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.wallStartTime = time.Now()

	log.Printf("Dispatcher starting with logical time, %d specs, first spec RPM: %d",
		len(d.specs), d.specs[0].RPM)

	// Semaphore for limiting concurrent in-flight requests
	sem := make(chan struct{}, 500)

	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			log.Printf("Dispatcher stopping (context cancelled), waiting for in-flight requests...")
			wg.Wait()
			return ctx.Err()
		default:
		}

		// Check if we've exhausted all specs
		if d.currentSpec >= len(d.specs) {
			log.Printf("All specs exhausted after %d requests (logical time: %.1f hours, wall time: %.1f seconds)",
				d.totalRequests.Load(),
				float64(d.logicalTimeNS)/float64(time.Hour),
				time.Since(d.wallStartTime).Seconds())
			wg.Wait()
			return nil
		}

		// Calculate next logical timestamp using exponential inter-arrival
		interval := d.rng.ExpFloat64() * d.interArrivalTimeNS
		d.logicalTimeNS += int64(interval)

		// Check spec transition (based on logical time)
		d.maybeAdvanceSpec()

		// Check again after potential spec advance
		if d.currentSpec >= len(d.specs) {
			continue // Next iteration will catch this and exit properly
		}

		// Acquire semaphore slot (blocks if too many in-flight)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}

		// Create request with logical timestamp
		d.requestCounter++
		req := api.AppRequest{
			RequestID:   fmt.Sprintf("req-%d", d.requestCounter),
			TimestampNS: d.logicalTimeNS,
			SpecIndex:   d.currentSpec,
			TimeoutMS:   int(d.specs[d.currentSpec].TimeoutMS),
		}

		d.totalRequests.Add(1)

		wg.Add(1)
		go func(req api.AppRequest) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := d.sendRequest(ctx, req); err != nil {
				d.totalErrors.Add(1)
				// Log occasionally to avoid spam
				if d.totalErrors.Load()%1000 == 1 {
					log.Printf("Request error (total errors: %d): %v", d.totalErrors.Load(), err)
				}
			} else {
				d.totalResponses.Add(1)
			}
		}(req)

		// Log progress periodically
		if d.totalRequests.Load()%10000 == 0 {
			log.Printf("Progress: %d requests, logical time: %.2f hours, wall time: %.1fs, spec: %d/%d",
				d.totalRequests.Load(),
				float64(d.logicalTimeNS)/float64(time.Hour),
				time.Since(d.wallStartTime).Seconds(),
				d.currentSpec+1, len(d.specs))
		}
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
			log.Printf("Advanced to spec %d/%d: RPM=%d, ErrorRate=%.2f, Duration=%ds (logical time: %.2f hours)",
				d.currentSpec+1, len(d.specs), newSpec.RPM, newSpec.ErrorRate, newSpec.DurationS,
				float64(d.logicalTimeNS)/float64(time.Hour))
		}
	}
}

// sendRequest sends a request to the app container.
func (d *Dispatcher) sendRequest(ctx context.Context, req api.AppRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	// For Unix sockets, use a dummy host since the transport handles the connection
	var url string
	if len(d.appHost) > 0 && d.appHost[0] == '/' {
		url = "http://unix/request"
	} else {
		url = fmt.Sprintf("http://%s/request", d.appHost)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request creation error: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	// Decode response (we don't need to do much with it, just validate)
	var appResp api.AppResponse
	if err := json.NewDecoder(resp.Body).Decode(&appResp); err != nil {
		return fmt.Errorf("decode error: %w", err)
	}

	return nil
}

// GetStatus returns the current dispatcher status.
func (d *Dispatcher) GetStatus() api.LoadGenStatus {
	return api.LoadGenStatus{
		CurrentSpecIndex:   d.currentSpec,
		ElapsedSeconds:     time.Since(d.wallStartTime).Seconds(),
		LogicalTimeHours:   float64(d.logicalTimeNS) / float64(time.Hour),
		TotalRequests:      d.totalRequests.Load(),
		CurrentRPS:         d.calculateCurrentRPS(),
		TotalLogicalHours:  d.calculateTotalLogicalHours(),
	}
}

// calculateCurrentRPS estimates current wall-clock RPS.
func (d *Dispatcher) calculateCurrentRPS() float64 {
	elapsed := time.Since(d.wallStartTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(d.totalRequests.Load()) / elapsed
}

// calculateTotalLogicalHours returns the total duration of all specs in hours.
func (d *Dispatcher) calculateTotalLogicalHours() float64 {
	var totalSeconds int64
	for _, spec := range d.specs {
		totalSeconds += int64(spec.DurationS)
	}
	return float64(totalSeconds) / 3600.0
}
