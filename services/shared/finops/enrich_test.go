package finops

import (
	"errors"
	"testing"
)

func TestEnrich(t *testing.T) {
	knownCC := CostCenterInfo{CostCenterID: "cc-1", TeamID: "team-a", TeamName: "Alpha"}

	// A lookup that knows about cc-1 only, never errors.
	lookup := func(id string) (CostCenterInfo, bool, error) {
		if id == "cc-1" {
			return knownCC, true, nil
		}
		return CostCenterInfo{}, false, nil
	}

	errLookup := func(id string) (CostCenterInfo, bool, error) {
		return CostCenterInfo{}, false, errors.New("state store down")
	}

	tests := []struct {
		name      string
		item      LineItem
		lookup    LookupFunc
		wantErrIs error
		wantErr   bool
		wantCC    string
		wantTeam  string
	}{
		{
			name:     "happy path — tag present and known",
			item:     LineItem{ID: "li-1", Tags: map[string]string{CostCenterTag: "cc-1"}},
			lookup:   lookup,
			wantCC:   "cc-1",
			wantTeam: "team-a",
		},
		{
			name:      "missing cost-center tag → unmapped",
			item:      LineItem{ID: "li-2", Tags: map[string]string{"environment": "prod"}},
			lookup:    lookup,
			wantErrIs: ErrMissingCostCenter,
		},
		{
			name:      "empty cost-center tag value → unmapped",
			item:      LineItem{ID: "li-3", Tags: map[string]string{CostCenterTag: ""}},
			lookup:    lookup,
			wantErrIs: ErrMissingCostCenter,
		},
		{
			name:      "nil tags → unmapped",
			item:      LineItem{ID: "li-4", Tags: nil},
			lookup:    lookup,
			wantErrIs: ErrMissingCostCenter,
		},
		{
			name:      "unknown cost-center ID → unmapped",
			item:      LineItem{ID: "li-5", Tags: map[string]string{CostCenterTag: "cc-999"}},
			lookup:    lookup,
			wantErrIs: ErrUnknownCostCenter,
		},
		{
			name:    "lookup surfaces its own error (state store down)",
			item:    LineItem{ID: "li-6", Tags: map[string]string{CostCenterTag: "cc-1"}},
			lookup:  errLookup,
			wantErr: true,
		},
		{
			name: "extra tags are preserved on the enriched output",
			item: LineItem{
				ID:      "li-7",
				Day:     "2026-07-04",
				Service: "ec2",
				CostUSD: 12.34,
				Tags:    map[string]string{CostCenterTag: "cc-1", "environment": "prod"},
			},
			lookup:   lookup,
			wantCC:   "cc-1",
			wantTeam: "team-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Enrich(tt.item, tt.lookup)

			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("want error %v, got %v", tt.wantErrIs, err)
				}
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.CostCenterID != tt.wantCC {
				t.Errorf("CostCenterID: want %q, got %q", tt.wantCC, got.CostCenterID)
			}
			if got.TeamID != tt.wantTeam {
				t.Errorf("TeamID: want %q, got %q", tt.wantTeam, got.TeamID)
			}
			if got.LineItem.ID != tt.item.ID {
				t.Errorf("original LineItem lost: want ID %q, got %q", tt.item.ID, got.LineItem.ID)
			}
			// Ensure original tags are preserved on the embedded LineItem.
			for k, v := range tt.item.Tags {
				if got.LineItem.Tags[k] != v {
					t.Errorf("tag %q lost: want %q, got %q", k, v, got.LineItem.Tags[k])
				}
			}
		})
	}
}
