// Package finops defines the shared domain types and pure logic used by every
// Dapr-FinOps service. Everything in this package is stateless and Dapr-free
// so it can be unit-tested in isolation.
package finops

// LineItem is the minimum-viable synthetic FinOps line item, matching the
// schema decided in T7. Real cloud billing extracts have ~30 columns; we
// intentionally carry only what the demo needs (enrich, aggregate, detect).
type LineItem struct {
	ID       string            `json:"line_item_id"`
	Day      string            `json:"day"` // YYYY-MM-DD, no timezone
	Service  string            `json:"service"`
	CostUSD  float64           `json:"cost_usd"`
	Quantity float64           `json:"quantity"`
	Unit     string            `json:"unit"`
	Tags     map[string]string `json:"tags"`
}

// EnrichedLineItem is a LineItem with ownership metadata attached.
// The cost_center_id is also hoisted out of tags so downstream queries can
// index on it without JSON extraction.
type EnrichedLineItem struct {
	LineItem
	CostCenterID string `json:"cost_center_id"`
	TeamID       string `json:"team_id"`
	TeamName     string `json:"team_name"`
}

// CostCenterInfo is the entry in the cost-center → team lookup, seeded into
// the Dapr state store on ingest-svc startup and read on every enrichment.
type CostCenterInfo struct {
	CostCenterID string `json:"cost_center_id"`
	TeamID       string `json:"team_id"`
	TeamName     string `json:"team_name"`
}
