package tests

import (
	"container/heap"
	"math"
	"testing"
	"time"

	"github.com/codemartial/levee"
)

type completion struct {
	sequence int64
	done     time.Time
	duration time.Duration
	success  bool
}

type completionHeap []completion

func (h completionHeap) Len() int { return len(h) }
func (h completionHeap) Less(i, j int) bool {
	if h[i].done.Equal(h[j].done) {
		return h[i].sequence < h[j].sequence
	}
	return h[i].done.Before(h[j].done)
}
func (h completionHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *completionHeap) Push(x any)   { *h = append(*h, x.(completion)) }
func (h *completionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type driverStats struct {
	attempted       int
	admitted        int
	rejected        int
	succeeded       int
	failed          int
	rejectedByClass map[string]int
	transitions     map[levee.Trigger]int
}

type driver struct {
	t       *testing.T
	levee   *levee.Levee
	state   levee.State
	pending completionHeap
	nextSeq int64
	stats   driverStats
}

func newDriver(t *testing.T, l *levee.Levee) *driver {
	t.Helper()
	d := &driver{
		t:     t,
		levee: l,
		state: l.State(),
		stats: driverStats{
			rejectedByClass: make(map[string]int),
			transitions:     make(map[levee.Trigger]int),
		},
	}
	heap.Init(&d.pending)
	d.checkSnapshot()
	return d
}

func (d *driver) observe(sc levee.StateChange) {
	d.t.Helper()
	if !legalTransition(d.state, sc.State) {
		d.t.Fatalf("illegal state transition %s -> %s", d.state, sc.State)
	}
	if sc.State == d.state && sc.Trigger != levee.TriggerNone {
		d.t.Fatalf("steady state %s reported trigger %s", sc.State, sc.Trigger)
	}
	if sc.State != d.state {
		if sc.Trigger == levee.TriggerNone {
			d.t.Fatalf("transition %s -> %s has no trigger", d.state, sc.State)
		}
		d.stats.transitions[sc.Trigger]++
	}
	d.state = sc.State
}

func (d *driver) checkSnapshot() {
	d.t.Helper()
	s := d.levee.Snapshot()
	if s.State != d.state {
		d.t.Fatalf("snapshot state = %s, observed state = %s", s.State, d.state)
	}
	if s.Inflight != int64(len(d.pending)) {
		d.t.Fatalf("snapshot inflight = %d, pending completions = %d", s.Inflight, len(d.pending))
	}
	if s.Inflight < 0 {
		d.t.Fatalf("negative inflight: %d", s.Inflight)
	}
	if s.Capped && s.Limit < 1 {
		d.t.Fatalf("capped snapshot has invalid limit %d", s.Limit)
	}
	if !s.Capped && s.Limit != 0 {
		d.t.Fatalf("uncapped snapshot has limit %d", s.Limit)
	}
	for name, value := range map[string]float64{
		"capacity":     s.EstimatedCapacity,
		"error rate":   s.ErrorRate,
		"error bound":  s.ErrorLowerBound,
		"surge strain": s.Surge.Strain,
		"surge proven": s.Surge.ProvenExcess,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			d.t.Fatalf("snapshot %s is not finite: %v", name, value)
		}
	}
}

func (d *driver) advance(ts time.Time) {
	d.t.Helper()
	for d.pending.Len() > 0 && !d.pending[0].done.After(ts) {
		c := heap.Pop(&d.pending).(completion)
		var sc levee.StateChange
		if c.success {
			sc = d.levee.Success(c.done, c.duration)
			d.stats.succeeded++
		} else {
			sc = d.levee.Fail(c.done, c.duration)
			d.stats.failed++
		}
		d.observe(sc)
		d.checkSnapshot()
	}
}

func (d *driver) arrive(ts time.Time, latency time.Duration, success bool, class string) bool {
	d.t.Helper()
	d.advance(ts)
	d.stats.attempted++
	sc, err := d.levee.Start(ts)
	d.observe(sc)
	if err != nil {
		d.stats.rejected++
		d.stats.rejectedByClass[class]++
		d.checkSnapshot()
		return false
	}
	d.stats.admitted++
	d.nextSeq++
	heap.Push(&d.pending, completion{
		sequence: d.nextSeq,
		done:     ts.Add(latency),
		duration: latency,
		success:  success,
	})
	d.checkSnapshot()
	return true
}

func (d *driver) flush() {
	d.t.Helper()
	for d.pending.Len() > 0 {
		d.advance(d.pending[0].done)
	}
}

func legalTransition(from, to levee.State) bool {
	switch from {
	case levee.CLOSED:
		return to == levee.CLOSED || to == levee.THROTTLED
	case levee.THROTTLED:
		return to == levee.THROTTLED || to == levee.CLOSED || to == levee.OPEN
	case levee.OPEN:
		return to == levee.OPEN || to == levee.HALF_OPEN
	case levee.HALF_OPEN:
		return to == levee.HALF_OPEN || to == levee.CLOSED || to == levee.OPEN
	default:
		return false
	}
}

func warmHealthy(t *testing.T, l *levee.Levee, start time.Time, samples int, interval, latency time.Duration) time.Time {
	t.Helper()
	ts := start
	for range samples {
		sc, err := l.Start(ts)
		if err != nil {
			t.Fatalf("healthy warmup rejected at %s: %v", ts.Sub(start), err)
		}
		if sc.Trigger != levee.TriggerNone {
			t.Fatalf("healthy warmup transitioned: %+v", sc)
		}
		sc = l.Success(ts.Add(latency), latency)
		if sc.Trigger != levee.TriggerNone {
			t.Fatalf("healthy warmup completion transitioned: %+v", sc)
		}
		ts = ts.Add(interval)
	}
	return ts
}
