package mesh

import (
	"math"
	"testing"

	"github.com/codemartial/levee/benchmarks/distributed/backend"
)

func TestStorefrontTopologySanity(t *testing.T) {
	topo := StorefrontTopology()
	if err := topo.Validate(); err != nil {
		t.Fatalf("storefront topology invalid: %v", err)
	}
	steady, err := topo.SteadyStateRPS()
	if err != nil {
		t.Fatalf("steady state: %v", err)
	}
	want := map[string]float64{
		"edge-api":  300,
		"admin-api": 20,
		"catalog":   250,
		"pricing":   250,
		"orders":    75.0 / 0.7, // cycle gain 0.3
		"inventory": 10 + 125 + 75.0/0.7,
		"payments":  75.0 / 0.7,
		"db":        250 + (10 + 125 + 75.0/0.7) + 75.0/0.7,
		"notify":    75.0 / 0.7,
		"audit":     6,
	}
	for name, w := range want {
		if got := steady[name]; math.Abs(got-w) > 0.1 {
			t.Errorf("steady RPS %s: got %.2f want %.2f", name, got, w)
		}
	}
}

func TestValidateRejectsSyncCycle(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		{Name: "a", EntryRPS: 10, PerReplicaRPS: 100, MinReplicas: 1, MaxReplicas: 4, P50MS: 5, P99MS: 20,
			Edges: []EdgeSpec{{Callee: "b", Prob: 0.5}}},
		{Name: "b", PerReplicaRPS: 100, MinReplicas: 1, MaxReplicas: 4, P50MS: 5, P99MS: 20,
			Edges: []EdgeSpec{{Callee: "a", Prob: 0.5}}},
	})
	if err := topo.Validate(); err == nil {
		t.Error("expected sync-cycle validation error")
	}
}

func TestValidateRejectsMissingDivider(t *testing.T) {
	topo := NewTopology([]NodeSpec{
		{Name: "big", EntryRPS: 500, PerReplicaRPS: 200, MinReplicas: 2, MaxReplicas: 8, P50MS: 5, P99MS: 20,
			Edges: []EdgeSpec{{Callee: "tiny", Prob: 0.5}}},
		{Name: "tiny", PerReplicaRPS: 10, MinReplicas: 1, MaxReplicas: 2, P50MS: 5, P99MS: 20},
	})
	if err := topo.Validate(); err == nil {
		t.Error("expected divider-rule validation error for big->tiny at p=0.5")
	}
}

func TestLatencyProfileMeanMatchesBackend(t *testing.T) {
	cases := [][2]float64{{5, 20}, {10, 40}, {20, 80}, {50, 150}}
	for _, c := range cases {
		got := newLatencyProfile(c[0], c[1], defaultTimeoutMS).meanMS()
		want := backend.NewLatencyProfile(c[0], c[1], defaultTimeoutMS).MeanLatencyMS()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("meanMS(p50=%v,p99=%v): got %v want %v", c[0], c[1], got, want)
		}
	}
}
