package finops

import (
	"testing"
	"time"
)

func TestRollupKey(t *testing.T) {
	r := Rollup{Day: "2026-07-04", TeamID: "team-x", Service: "ec2"}
	if got, want := r.Key(), "rollup:2026-07-04:team-x:ec2"; got != want {
		t.Fatalf("Key: want %q, got %q", want, got)
	}
}

func TestFromLineItem(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	item := EnrichedLineItem{
		LineItem: LineItem{
			ID:       "li-1",
			Day:      "2026-07-04",
			Service:  "ec2",
			CostUSD:  10.0,
			Quantity: 5.0,
		},
		CostCenterID: "cc-1",
		TeamID:       "team-x",
		TeamName:     "Team X",
	}
	r := FromLineItem(item, now)

	if r.CostUSD != 10.0 || r.Quantity != 5.0 || r.Count != 1 {
		t.Errorf("FromLineItem numerics wrong: %+v", r)
	}
	if r.LastUpdated != "2026-07-04T12:00:00Z" {
		t.Errorf("LastUpdated: got %q", r.LastUpdated)
	}
	if r.Key() != "rollup:2026-07-04:team-x:ec2" {
		t.Errorf("Key: got %q", r.Key())
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name         string
		base         Rollup
		delta        Rollup
		wantCost     float64
		wantQty      float64
		wantCount    int
		wantLastUpd  string
	}{
		{
			name:        "into empty base (first item for this key)",
			base:        Rollup{},
			delta:       Rollup{Day: "d", TeamID: "t", Service: "ec2", CostUSD: 5, Quantity: 2, Count: 1, LastUpdated: "T1"},
			wantCost:    5,
			wantQty:     2,
			wantCount:   1,
			wantLastUpd: "T1",
		},
		{
			name: "into existing rollup — sums and takes delta's LastUpdated",
			base: Rollup{
				Day: "d", TeamID: "t", Service: "ec2",
				CostUSD: 100, Quantity: 10, Count: 5, LastUpdated: "T0",
			},
			delta: Rollup{
				Day: "d", TeamID: "t", Service: "ec2",
				CostUSD: 25, Quantity: 3, Count: 1, LastUpdated: "T1",
			},
			wantCost:    125,
			wantQty:     13,
			wantCount:   6,
			wantLastUpd: "T1",
		},
		{
			name: "zero-value delta is a no-op on totals",
			base: Rollup{
				Day: "d", TeamID: "t", Service: "ec2",
				CostUSD: 50, Quantity: 4, Count: 2, LastUpdated: "T0",
			},
			delta:       Rollup{Day: "d", TeamID: "t", Service: "ec2", LastUpdated: "T1"},
			wantCost:    50,
			wantQty:     4,
			wantCount:   2,
			wantLastUpd: "T1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.base.Merge(tt.delta)
			if got.CostUSD != tt.wantCost {
				t.Errorf("CostUSD: want %v, got %v", tt.wantCost, got.CostUSD)
			}
			if got.Quantity != tt.wantQty {
				t.Errorf("Quantity: want %v, got %v", tt.wantQty, got.Quantity)
			}
			if got.Count != tt.wantCount {
				t.Errorf("Count: want %d, got %d", tt.wantCount, got.Count)
			}
			if got.LastUpdated != tt.wantLastUpd {
				t.Errorf("LastUpdated: want %q, got %q", tt.wantLastUpd, got.LastUpdated)
			}
		})
	}
}

// Idempotency at the domain layer: merging N times still yields the same
// per-item accumulation. Real idempotency at runtime is enforced by the
// "processed:<line-item-id>" FirstWrite check in rollup-svc — this test just
// pins the algebra.
func TestMergeAssociativityLike(t *testing.T) {
	base := Rollup{Day: "d", TeamID: "t", Service: "ec2"}
	delta := Rollup{Day: "d", TeamID: "t", Service: "ec2", CostUSD: 3, Count: 1, LastUpdated: "T"}

	out := base
	for i := 0; i < 4; i++ {
		out = out.Merge(delta)
	}
	if out.CostUSD != 12 || out.Count != 4 {
		t.Errorf("4 merges: want cost=12 count=4, got cost=%v count=%d", out.CostUSD, out.Count)
	}
}
