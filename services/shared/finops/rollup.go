package finops

import (
	"fmt"
	"time"
)

// Rollup is an aggregation of enriched line items for a specific
// (day, team, service) triple. Persisted in state-postgres under
// key `rollup:<day>:<team_id>:<service>`.
type Rollup struct {
	Day         string  `json:"day"`
	TeamID      string  `json:"team_id"`
	TeamName    string  `json:"team_name"`
	Service     string  `json:"service"`
	CostUSD     float64 `json:"cost_usd"`
	Quantity    float64 `json:"quantity"`
	Count       int     `json:"count"`
	LastUpdated string  `json:"last_updated"` // RFC3339
}

// Key is the canonical state-store key for this rollup. Stable across restarts.
func (r Rollup) Key() string {
	return fmt.Sprintf("rollup:%s:%s:%s", r.Day, r.TeamID, r.Service)
}

// FromLineItem produces a single-item Rollup from an enriched line item.
// Callers Merge this into whatever is currently in state.
func FromLineItem(item EnrichedLineItem, now time.Time) Rollup {
	return Rollup{
		Day:         item.Day,
		TeamID:      item.TeamID,
		TeamName:    item.TeamName,
		Service:     item.Service,
		CostUSD:     item.CostUSD,
		Quantity:    item.Quantity,
		Count:       1,
		LastUpdated: now.UTC().Format(time.RFC3339),
	}
}

// Merge combines two rollups for the same key. The caller is responsible for
// ensuring the keys match (this is a pure combinator, not a validator).
// The output carries the delta's LastUpdated timestamp; the identifying
// dimensions (day/team/service) also come from the delta so an empty
// zero-value base can Merge cleanly.
func (base Rollup) Merge(delta Rollup) Rollup {
	return Rollup{
		Day:         delta.Day,
		TeamID:      delta.TeamID,
		TeamName:    delta.TeamName,
		Service:     delta.Service,
		CostUSD:     base.CostUSD + delta.CostUSD,
		Quantity:    base.Quantity + delta.Quantity,
		Count:       base.Count + delta.Count,
		LastUpdated: delta.LastUpdated,
	}
}
