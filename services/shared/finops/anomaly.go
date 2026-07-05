package finops

import (
	"fmt"
	"time"
)

// DetectorConfig captures the tunable thresholds. Passed in so Detect stays
// pure and testable without env-var lookups.
type DetectorConfig struct {
	// PctThreshold: multiplier over baseline mean that triggers an anomaly.
	// 1.5 means "cost must be at least 150% of the baseline mean".
	PctThreshold float64

	// MinBaselineUSD: noise floor. If the baseline mean is below this,
	// no anomaly is emitted even if the multiplier is exceeded. Prevents
	// teams with baseline of $2 tripping on a $4 spike.
	MinBaselineUSD float64
}

// Anomaly is what Detect returns when a rollup exceeds the configured
// threshold vs. its historical baseline. Also the payload published on the
// `anomaly.detected` pub/sub topic.
type Anomaly struct {
	Day             string  `json:"day"`
	TeamID          string  `json:"team_id"`
	TeamName        string  `json:"team_name"`
	Service         string  `json:"service"`
	ActualCostUSD   float64 `json:"actual_cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	DeltaUSD        float64 `json:"delta_usd"`
	DeltaPct        float64 `json:"delta_pct"`
	ThresholdPct    float64 `json:"threshold_pct"`
	BaselineDays    int     `json:"baseline_days"`
	DetectedAt      string  `json:"detected_at"` // RFC3339
	Reason          string  `json:"reason"`
}

// ID is the deterministic identifier for an anomaly. Same (day, team, service)
// always produces the same ID so idempotency-via-FirstWrite works across
// re-detections and event-driven-vs-batch triggers.
func (a Anomaly) ID() string {
	return fmt.Sprintf("anomaly:%s:%s:%s", a.Day, a.TeamID, a.Service)
}

// Detect compares the current rollup against a history of prior rollups for
// the same (team, service). Returns a populated *Anomaly if:
//   - current cost > 0, and
//   - history is non-empty, and
//   - baseline mean >= cfg.MinBaselineUSD (noise floor), and
//   - current cost >= baseline mean * cfg.PctThreshold
//
// Otherwise returns nil. Callers are responsible for ensuring history items
// share the same team/service as current — Detect does not validate that.
func Detect(current Rollup, history []Rollup, cfg DetectorConfig, now time.Time) *Anomaly {
	if current.CostUSD <= 0 || len(history) == 0 {
		return nil
	}

	var sum float64
	for _, r := range history {
		sum += r.CostUSD
	}
	baseline := sum / float64(len(history))

	if baseline < cfg.MinBaselineUSD {
		return nil
	}
	if current.CostUSD < baseline*cfg.PctThreshold {
		return nil
	}

	deltaUSD := current.CostUSD - baseline
	deltaPct := (deltaUSD / baseline) * 100.0
	thresholdPct := (cfg.PctThreshold - 1.0) * 100.0

	return &Anomaly{
		Day:             current.Day,
		TeamID:          current.TeamID,
		TeamName:        current.TeamName,
		Service:         current.Service,
		ActualCostUSD:   current.CostUSD,
		BaselineCostUSD: baseline,
		DeltaUSD:        deltaUSD,
		DeltaPct:        deltaPct,
		ThresholdPct:    thresholdPct,
		BaselineDays:    len(history),
		DetectedAt:      now.UTC().Format(time.RFC3339),
		Reason: fmt.Sprintf(
			"cost %.0f%% over %dd-baseline; threshold %.0f%%",
			deltaPct, len(history), thresholdPct,
		),
	}
}
