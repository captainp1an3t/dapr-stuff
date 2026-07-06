package finops

import "testing"

func TestIdleResource_ID_isDeterministic(t *testing.T) {
	r := IdleResource{
		TeamID:          "team-payments",
		ResourceID:      "vol-abc123",
		SuggestedAction: "delete",
	}
	if got, want := r.ID(), "optimisation:team-payments:vol-abc123:delete"; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}

	// Same values → same ID (idempotency contract).
	other := IdleResource{
		TeamID:          "team-payments",
		ResourceID:      "vol-abc123",
		SuggestedAction: "delete",
		// Unrelated fields don't affect identity.
		MonthlyWasteUSD: 999,
		DaysIdle:        45,
	}
	if r.ID() != other.ID() {
		t.Errorf("ID differed on unrelated fields: %q vs %q", r.ID(), other.ID())
	}
}

func TestIdleResource_ID_distinctPerAction(t *testing.T) {
	// Same resource, different suggested action → different workflow instance.
	// Rationale: an org might decline "delete" today then approve "downsize"
	// next week for the same resource.
	base := IdleResource{TeamID: "t", ResourceID: "r"}
	delete := base
	delete.SuggestedAction = "delete"
	downsize := base
	downsize.SuggestedAction = "downsize"
	if delete.ID() == downsize.ID() {
		t.Errorf("expected distinct IDs across actions, both were %q", delete.ID())
	}
}
