package finops

import (
	"testing"
	"time"
)

func TestDetect(t *testing.T) {
	now := time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC)
	cfg := DetectorConfig{PctThreshold: 1.5, MinBaselineUSD: 10.0}

	// Build a helper: N days of history each with the same cost, and a "today"
	// rollup we vary in the tests. Team/service labels match current, as the
	// caller is expected to ensure.
	history := func(days int, costEach float64) []Rollup {
		out := make([]Rollup, days)
		for i := 0; i < days; i++ {
			out[i] = Rollup{
				Day: "d", TeamID: "t", TeamName: "T", Service: "ec2",
				CostUSD: costEach, Count: 1,
			}
		}
		return out
	}
	current := func(cost float64) Rollup {
		return Rollup{
			Day: "2026-07-04", TeamID: "t", TeamName: "T", Service: "ec2",
			CostUSD: cost, Count: 1,
		}
	}

	tests := []struct {
		name     string
		current  Rollup
		history  []Rollup
		wantHit  bool
		wantMean float64
	}{
		{
			name:     "cost 2x baseline, above floor → anomaly",
			current:  current(200),
			history:  history(7, 100),
			wantHit:  true,
			wantMean: 100,
		},
		{
			name:     "cost exactly at threshold (baseline * 1.5) → anomaly (>=)",
			current:  current(150),
			history:  history(7, 100),
			wantHit:  true,
			wantMean: 100,
		},
		{
			name:    "cost below threshold — 1.4x baseline → nil",
			current: current(140),
			history: history(7, 100),
			wantHit: false,
		},
		{
			name:    "baseline below noise floor → nil even at 10x",
			current: current(50),
			history: history(7, 5), // mean = 5, below MinBaselineUSD = 10
			wantHit: false,
		},
		{
			name:    "empty history → nil",
			current: current(200),
			history: nil,
			wantHit: false,
		},
		{
			name:    "zero current cost → nil",
			current: current(0),
			history: history(7, 100),
			wantHit: false,
		},
		{
			name:    "history with mixed values — mean computed correctly",
			current: current(300),
			// mean = (50+100+150+50+100+150+200) / 7 = 800/7 ≈ 114.29
			// threshold = 114.29 * 1.5 ≈ 171.43 → 300 > that → anomaly
			history: []Rollup{
				{CostUSD: 50}, {CostUSD: 100}, {CostUSD: 150},
				{CostUSD: 50}, {CostUSD: 100}, {CostUSD: 150},
				{CostUSD: 200},
			},
			wantHit:  true,
			wantMean: 800.0 / 7.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.current, tt.history, cfg, now)
			if tt.wantHit && got == nil {
				t.Fatalf("wanted anomaly, got nil")
			}
			if !tt.wantHit && got != nil {
				t.Fatalf("wanted nil, got %+v", got)
			}
			if !tt.wantHit {
				return
			}
			if got.ActualCostUSD != tt.current.CostUSD {
				t.Errorf("ActualCostUSD: want %v, got %v", tt.current.CostUSD, got.ActualCostUSD)
			}
			if diff := got.BaselineCostUSD - tt.wantMean; diff > 0.01 || diff < -0.01 {
				t.Errorf("BaselineCostUSD: want ~%v, got %v", tt.wantMean, got.BaselineCostUSD)
			}
			if got.BaselineDays != len(tt.history) {
				t.Errorf("BaselineDays: want %d, got %d", len(tt.history), got.BaselineDays)
			}
			if got.Day != tt.current.Day {
				t.Errorf("Day: want %q, got %q", tt.current.Day, got.Day)
			}
			if got.Reason == "" {
				t.Errorf("Reason should be populated")
			}
			if got.DetectedAt != "2026-07-04T15:00:00Z" {
				t.Errorf("DetectedAt: got %q", got.DetectedAt)
			}
		})
	}
}

func TestAnomalyID(t *testing.T) {
	a := Anomaly{Day: "2026-07-04", TeamID: "team-payments", Service: "ec2"}
	if got, want := a.ID(), "anomaly:2026-07-04:team-payments:ec2"; got != want {
		t.Errorf("ID: want %q, got %q", want, got)
	}
}
