// Package mesh implements a multi-node service mesh simulation benchmark
// where every node runs admission governors for inbound and outbound traffic.
package mesh

import (
	"fmt"
	"math"
)

// EdgeSpec is one outbound dependency of a node.
type EdgeSpec struct {
	Callee string
	Prob   float64 // probability that a processed request calls this edge
	Async  bool    // fire-and-forget; outcome never affects the parent
}

// NodeSpec is the static description of one mesh node.
type NodeSpec struct {
	Name          string
	EntryRPS      float64 // external arrival rate; 0 = internal node
	PerReplicaRPS int
	MinReplicas   int
	MaxReplicas   int
	P50MS         float64
	P99MS         float64
	Edges         []EdgeSpec
}

// Topology is a validated set of nodes and edges.
type Topology struct {
	Nodes []NodeSpec
	index map[string]int
}

// NewTopology builds a topology and its name index.
func NewTopology(nodes []NodeSpec) *Topology {
	t := &Topology{Nodes: nodes, index: make(map[string]int, len(nodes))}
	for i, n := range nodes {
		t.index[n.Name] = i
	}
	return t
}

// NodeIndex returns the index of a node by name, or -1.
func (t *Topology) NodeIndex(name string) int {
	if i, ok := t.index[name]; ok {
		return i
	}
	return -1
}

// StorefrontTopology returns the 10-node reference mesh: two entry points,
// depth-4 chains, fan-out, fan-in at db, and an async callback cycle.
func StorefrontTopology() *Topology {
	return NewTopology([]NodeSpec{
		{Name: "edge-api", EntryRPS: 300, PerReplicaRPS: 100, MinReplicas: 2, MaxReplicas: 16, P50MS: 5, P99MS: 20,
			Edges: []EdgeSpec{
				{Callee: "catalog", Prob: 0.80},
				{Callee: "orders", Prob: 0.25},
				{Callee: "audit", Prob: 0.02, Async: true},
			}},
		{Name: "admin-api", EntryRPS: 20, PerReplicaRPS: 25, MinReplicas: 1, MaxReplicas: 4, P50MS: 10, P99MS: 40,
			Edges: []EdgeSpec{
				{Callee: "catalog", Prob: 0.50},
				{Callee: "inventory", Prob: 0.50},
			}},
		{Name: "catalog", PerReplicaRPS: 100, MinReplicas: 2, MaxReplicas: 12, P50MS: 10, P99MS: 40,
			Edges: []EdgeSpec{
				{Callee: "pricing", Prob: 1.00},
				{Callee: "inventory", Prob: 0.50},
			}},
		{Name: "pricing", PerReplicaRPS: 100, MinReplicas: 2, MaxReplicas: 12, P50MS: 8, P99MS: 30,
			Edges: []EdgeSpec{
				{Callee: "db", Prob: 1.00},
			}},
		{Name: "orders", PerReplicaRPS: 50, MinReplicas: 2, MaxReplicas: 12, P50MS: 15, P99MS: 60,
			Edges: []EdgeSpec{
				{Callee: "inventory", Prob: 1.00},
				{Callee: "payments", Prob: 1.00},
			}},
		{Name: "inventory", PerReplicaRPS: 100, MinReplicas: 2, MaxReplicas: 12, P50MS: 10, P99MS: 40,
			Edges: []EdgeSpec{
				{Callee: "db", Prob: 1.00},
			}},
		{Name: "payments", PerReplicaRPS: 50, MinReplicas: 2, MaxReplicas: 12, P50MS: 20, P99MS: 80,
			Edges: []EdgeSpec{
				{Callee: "db", Prob: 1.00},
				{Callee: "notify", Prob: 1.00, Async: true},
			}},
		{Name: "db", PerReplicaRPS: 200, MinReplicas: 3, MaxReplicas: 16, P50MS: 5, P99MS: 25},
		{Name: "notify", PerReplicaRPS: 50, MinReplicas: 2, MaxReplicas: 12, P50MS: 10, P99MS: 40,
			Edges: []EdgeSpec{
				{Callee: "orders", Prob: 0.30, Async: true}, // callback cycle
			}},
		{Name: "audit", PerReplicaRPS: 10, MinReplicas: 1, MaxReplicas: 2, P50MS: 20, P99MS: 100},
	})
}

// SteadyStateRPS solves per-node inbound RPS by fixed-point iteration.
// Async edges count too: callbacks consume callee capacity like any call.
func (t *Topology) SteadyStateRPS() (map[string]float64, error) {
	rates := make([]float64, len(t.Nodes))
	for iter := 0; iter < 200; iter++ {
		maxDelta := 0.0
		for i := range t.Nodes {
			r := t.Nodes[i].EntryRPS
			for j := range t.Nodes {
				for _, e := range t.Nodes[j].Edges {
					if t.index[e.Callee] == i {
						r += e.Prob * rates[j]
					}
				}
			}
			maxDelta = math.Max(maxDelta, math.Abs(r-rates[i]))
			rates[i] = r
		}
		if maxDelta < 0.01 {
			out := make(map[string]float64, len(t.Nodes))
			for i, n := range t.Nodes {
				out[n.Name] = rates[i]
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("steady-state RPS did not converge: sync cycle or async gain >= 1")
}

// Validate checks capacity envelopes, the high-to-low divider rule,
// and that sync edges form a DAG.
func (t *Topology) Validate() error {
	steady, err := t.SteadyStateRPS()
	if err != nil {
		return err
	}
	for _, n := range t.Nodes {
		rps := steady[n.Name]
		needed := int(math.Ceil(rps / (0.70 * float64(n.PerReplicaRPS))))
		if needed > n.MaxReplicas {
			return fmt.Errorf("node %s: steady %.1f RPS needs %d replicas, max is %d", n.Name, rps, needed, n.MaxReplicas)
		}
		maxCap := 0.85 * float64(n.MaxReplicas*n.PerReplicaRPS)
		if rps > maxCap {
			return fmt.Errorf("node %s: steady %.1f RPS exceeds 85%% of max capacity %.1f", n.Name, rps, maxCap)
		}
		for _, e := range n.Edges {
			callee := t.Nodes[t.index[e.Callee]]
			calleeMax := float64(callee.MaxReplicas * callee.PerReplicaRPS)
			if rps > 2*calleeMax && e.Prob > 0.05 {
				return fmt.Errorf("edge %s->%s: high-throughput caller needs divider p <= 0.05, got %.2f", n.Name, e.Callee, e.Prob)
			}
		}
	}
	return t.checkSyncDAG()
}

// checkSyncDAG rejects cycles over sync edges (they would deadlock requests).
func (t *Topology) checkSyncDAG() error {
	const (
		unvisited = 0
		inStack   = 1
		done      = 2
	)
	state := make([]int, len(t.Nodes))
	var visit func(i int) error
	visit = func(i int) error {
		state[i] = inStack
		for _, e := range t.Nodes[i].Edges {
			if e.Async {
				continue
			}
			j := t.index[e.Callee]
			switch state[j] {
			case inStack:
				return fmt.Errorf("sync cycle through %s -> %s", t.Nodes[i].Name, e.Callee)
			case unvisited:
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		state[i] = done
		return nil
	}
	for i := range t.Nodes {
		if state[i] == unvisited {
			if err := visit(i); err != nil {
				return err
			}
		}
	}
	return nil
}

// SubtreeMeanLatencyMS approximates the expected end-to-end latency of a call
// into node: local mean plus the deepest probability-weighted sync branch.
func (t *Topology) SubtreeMeanLatencyMS(name string, hopMS float64) float64 {
	memo := make(map[int]float64)
	var solve func(i int) float64
	solve = func(i int) float64 {
		if v, ok := memo[i]; ok {
			return v
		}
		n := t.Nodes[i]
		v := newLatencyProfile(n.P50MS, n.P99MS, defaultTimeoutMS).meanMS()
		deepest := 0.0
		for _, e := range n.Edges {
			if e.Async {
				continue
			}
			d := e.Prob * (hopMS + solve(t.index[e.Callee]))
			deepest = math.Max(deepest, d)
		}
		v += deepest
		memo[i] = v
		return v
	}
	return solve(t.index[name])
}
