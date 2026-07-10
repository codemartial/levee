package mesh

import (
	"testing"

	"github.com/codemartial/levee/benchmarks"
)

func TestRoleTrackerDurations(t *testing.T) {
	rt := &RoleTracker{Name: "test"}
	rt.Observe(bucketClosed, 0)
	rt.Observe(bucketClosed, 1000) // repeated same-state observation
	rt.Observe(bucketThrottled, 3000)
	rt.Observe(bucketThrottled, 4000)
	rt.Observe(bucketOpen, 5000)
	rt.Observe(bucketClosed, 9000)
	rt.Flush(10000)

	if rt.DurNS[bucketClosed] != 3000+1000 {
		t.Errorf("closed: got %d want 4000", rt.DurNS[bucketClosed])
	}
	if rt.DurNS[bucketThrottled] != 2000 {
		t.Errorf("throttled: got %d want 2000", rt.DurNS[bucketThrottled])
	}
	if rt.DurNS[bucketOpen] != 4000 {
		t.Errorf("open: got %d want 4000", rt.DurNS[bucketOpen])
	}
	if rt.Transitions != 3 {
		t.Errorf("transitions: got %d want 3", rt.Transitions)
	}
}

func TestRoleTrackerFlushOnly(t *testing.T) {
	rt := &RoleTracker{}
	rt.Flush(5000)
	if rt.DurNS[bucketClosed] != 5000 {
		t.Errorf("unobserved role should default to CLOSED for the full span, got %d", rt.DurNS[bucketClosed])
	}
}

func TestTokenBucket(t *testing.T) {
	tb := NewTokenBucket(10, 10)
	for i := 0; i < 10; i++ {
		if !tb.Allow(0) {
			t.Fatalf("allow %d should pass on a full bucket", i)
		}
	}
	if tb.Allow(0) {
		t.Error("11th request at t=0 should be rejected")
	}
	if !tb.Saturated(0) {
		t.Error("bucket should report saturated")
	}
	if !tb.Allow(1e9) {
		t.Error("after 1s refill, request should pass")
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	cl := NewConcurrencyLimiter(2)
	for i := 0; i < 2; i++ {
		if !cl.TryAcquire() {
			t.Fatalf("acquire %d should pass", i)
		}
	}
	if cl.TryAcquire() {
		t.Error("third acquire should fail")
	}
	if !cl.Saturated() {
		t.Error("limiter should report saturated")
	}
	cl.Release()
	if !cl.TryAcquire() {
		t.Error("acquire after release should pass")
	}
}

func TestStaticSizing(t *testing.T) {
	topo := StorefrontTopology()
	g := newStaticGovernor(topo.NodeIndex("edge-api"), topo, StaticBreaker|StaticLimiter)
	if got := g.instances[0].inBucket.ratePerSec; got != 85 { // 0.85 * 100 per replica
		t.Errorf("edge-api per-replica inbound rate: got %v want 85", got)
	}
	if len(g.instances) != topo.Nodes[topo.NodeIndex("edge-api")].MinReplicas {
		t.Errorf("static governor should start with MinReplicas instances, got %d", len(g.instances))
	}
	for i, spec := range topo.Nodes {
		br := staticNodeSizing(topo, i, StaticBreaker)
		full := staticNodeSizing(topo, i, StaticBreaker|StaticLimiter)
		if br.inboundRate > float64(spec.PerReplicaRPS) {
			t.Errorf("node %s: per-replica inbound rate %.1f above replica capacity %d", spec.Name, br.inboundRate, spec.PerReplicaRPS)
		}
		for e, edge := range spec.Edges {
			if br.edgeMaxInflight[e] < 2 {
				t.Errorf("edge %s->%s: maxInflight %d below floor", spec.Name, edge.Callee, br.edgeMaxInflight[e])
			}
			// Standalone: per-instance edge volumes are bounded by
			// PerReplicaRPS, far below 800 RPS, so every breaker gets the
			// BAU config per the static_cb.go derivations.
			if br.edgeCBConfig[e].FailureThreshold != benchmarks.StaticBAUConfig.FailureThreshold {
				t.Errorf("edge %s->%s: expected BAU config at per-instance volume", spec.Name, edge.Callee)
			}
			// Chained: sync edges narrow to dead-edge detection; async
			// edges keep BAU (dropped callbacks never fail the parent).
			want := deadEdgeCBConfig.FailureThreshold
			if edge.Async {
				want = benchmarks.StaticBAUConfig.FailureThreshold
			}
			if full.edgeCBConfig[e].FailureThreshold != want {
				t.Errorf("edge %s->%s: chained FailureThreshold got %d want %d",
					spec.Name, edge.Callee, full.edgeCBConfig[e].FailureThreshold, want)
			}
			t.Logf("edge %-22s perInstanceMaxInflight=%3d cbFailureThreshold standalone=%d chained=%d",
				spec.Name+"->"+edge.Callee, br.edgeMaxInflight[e],
				br.edgeCBConfig[e].FailureThreshold, full.edgeCBConfig[e].FailureThreshold)
		}
	}
}

func TestStaticParts(t *testing.T) {
	topo := StorefrontTopology()
	node := topo.NodeIndex("edge-api")
	br := newStaticGovernor(node, topo, StaticBreaker)
	if br.instances[0].inBucket != nil {
		t.Error("breaker-only governor should have no inbound bucket")
	}
	if br.instances[0].outCB == nil || br.instances[0].outLim != nil {
		t.Error("breaker-only governor should have breakers and no limiters")
	}
	lim := newStaticGovernor(node, topo, StaticLimiter)
	if lim.instances[0].inBucket == nil {
		t.Error("limiter-only governor should have an inbound bucket")
	}
	if lim.instances[0].outCB != nil || lim.instances[0].outLim == nil {
		t.Error("limiter-only governor should have limiters and no breakers")
	}
}
