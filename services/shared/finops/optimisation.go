package finops

import "fmt"

// IdleResource represents a cloud resource that appears to be wasted spend
// — running but not used enough to justify the cost. Fed into the
// OptimisationWorkflow, which asks the owning team to approve or reject
// a cleanup action.
//
// Deliberately different shape from Anomaly so we can honestly report at
// the notifier boundary on payload polymorphism. Two workflows, two
// domain types, one notification service. If the abstraction is good,
// the notifier stays simple; if it's thin, the notifier grows a switch
// statement.
type IdleResource struct {
	TeamID           string  `json:"team_id"`
	TeamName         string  `json:"team_name"`
	Service          string  `json:"service"`           // e.g. "ec2", "rds", "ebs"
	ResourceID       string  `json:"resource_id"`       // cloud-side identifier
	ResourceType     string  `json:"resource_type"`     // "EBS volume", "RDS instance", …
	MonthlyWasteUSD  float64 `json:"monthly_waste_usd"` // estimated savings from cleanup
	DaysIdle         int     `json:"days_idle"`         // how long it's been idle
	DetectedAt       string  `json:"detected_at"`       // RFC3339
	SuggestedAction  string  `json:"suggested_action"`  // "delete", "downsize", "stop"
}

// ID is the deterministic identifier. Same resource + same suggested action
// → same workflow instance. Different action against the same resource
// (e.g. "downsize" today, "delete" next month) → different workflows.
func (r IdleResource) ID() string {
	return fmt.Sprintf("optimisation:%s:%s:%s", r.TeamID, r.ResourceID, r.SuggestedAction)
}
