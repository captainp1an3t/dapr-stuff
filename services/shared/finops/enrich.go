package finops

import "errors"

// CostCenterTag is the line-item tag key that carries the cost-center ID.
const CostCenterTag = "cost-center"

// ErrMissingCostCenter is returned when a line item has no cost-center tag,
// or the tag is empty. Ingest counts these as "unmapped" (not "failed") —
// they represent real unallocated cloud spend, not application errors.
var ErrMissingCostCenter = errors.New("line item missing cost-center tag")

// ErrUnknownCostCenter is returned when the tag is present but the ID is not
// in the lookup table. Also "unmapped", not a hard failure.
var ErrUnknownCostCenter = errors.New("cost center not found in lookup")

// LookupFunc resolves a cost-center ID to team info. In production it hits
// the Dapr state store on state-redis; in tests it's an in-memory map.
type LookupFunc func(costCenterID string) (info CostCenterInfo, found bool, err error)

// Enrich attaches ownership metadata to a line item. Returns:
//   - ErrMissingCostCenter if the cost-center tag is absent or empty
//   - ErrUnknownCostCenter if the tag value has no matching lookup entry
//   - any error the lookup itself surfaces
func Enrich(item LineItem, lookup LookupFunc) (EnrichedLineItem, error) {
	id, ok := item.Tags[CostCenterTag]
	if !ok || id == "" {
		return EnrichedLineItem{}, ErrMissingCostCenter
	}
	info, found, err := lookup(id)
	if err != nil {
		return EnrichedLineItem{}, err
	}
	if !found {
		return EnrichedLineItem{}, ErrUnknownCostCenter
	}
	return EnrichedLineItem{
		LineItem:     item,
		CostCenterID: info.CostCenterID,
		TeamID:       info.TeamID,
		TeamName:     info.TeamName,
	}, nil
}
