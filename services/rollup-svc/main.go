package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
)

const (
	stateStore     = "state-postgres" // durable rollups + processed markers + anomaly markers
	pubsubName     = "pubsub-rabbitmq"
	anomalyTopic   = "anomaly.detected"
	maxETagRetries = 8
	seedFilePath   = "/seed/cost-centers.json" // shared with ingest-svc via ConfigMap
)

// The set of services we know about (matches the Python generator).
// Batch detection iterates the cross-product of known cost centers × services.
var knownServices = []string{"ec2", "s3", "rds", "lambda", "cloudfront"}

// stats is a lock-free counter set for /stats.
type stats struct {
	Received    atomic.Int64
	Applied     atomic.Int64 // rollup update succeeded
	Duplicate   atomic.Int64 // seen before, skipped
	BadEnv      atomic.Int64 // bad CloudEvent envelope
	Failed      atomic.Int64 // real errors (state store, retries exhausted)
	Detected    atomic.Int64 // anomalies detected + published this process lifetime
	DetectedDup atomic.Int64 // detections that hit an existing anomaly marker (idempotent)
}

// Dapr CloudEvent envelope. Only the fields we actually consume.
type cloudEvent struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// Dapr pub/sub response envelope. Returning "SUCCESS" ACKs the message;
// "RETRY" tells Dapr to redeliver; "DROP" ACKs but signals a poison message.
type subResponse struct {
	Status string `json:"status"`
}

var (
	success = mustJSON(subResponse{Status: "SUCCESS"})
	retry   = mustJSON(subResponse{Status: "RETRY"})
	drop    = mustJSON(subResponse{Status: "DROP"})
)

func main() {
	ctx := context.Background()

	dapr, err := daprd.NewClient()
	if err != nil {
		log.Fatalf("dapr client: %v", err)
	}
	defer dapr.Close()

	// Detector config — env-tunable so we can demo different sensitivities
	// without redeploying.
	detCfg := finops.DetectorConfig{
		PctThreshold:   envFloat("ANOMALY_PCT_THRESHOLD", 1.5),
		MinBaselineUSD: envFloat("ANOMALY_MIN_BASELINE_USD", 10.0),
	}
	baselineDays := envInt("ANOMALY_BASELINE_DAYS", 7)
	log.Printf("anomaly detector: threshold=%.2fx, min-baseline=$%.2f, baseline-days=%d",
		detCfg.PctThreshold, detCfg.MinBaselineUSD, baselineDays)

	s := &stats{}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats(s))
	mux.HandleFunc("/events/line-item-enriched", handleLineItemEnriched(ctx, dapr, s, detCfg, baselineDays))
	mux.HandleFunc("/detect", handleDetectBatch(ctx, dapr, s, detCfg, baselineDays))

	addr := ":" + envOr("PORT", "8080")
	log.Printf("rollup-svc listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"okay","service":"rollup-svc"}`))
}

func handleStats(s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"received":     s.Received.Load(),
			"applied":      s.Applied.Load(),
			"duplicate":    s.Duplicate.Load(),
			"bad_env":      s.BadEnv.Load(),
			"failed":       s.Failed.Load(),
			"detected":     s.Detected.Load(),
			"detected_dup": s.DetectedDup.Load(),
		})
	}
}

// handleLineItemEnriched processes one CloudEvent carrying an enriched line
// item. Semantics:
//   - malformed envelope or payload → DROP (poison message, ACK and skip)
//   - already-seen line item        → SUCCESS (idempotent, ACK)
//   - transient state store error   → RETRY (Dapr redelivers)
//   - rollup upsert exhausts ETag retries → RETRY
//   - success                       → run detection + SUCCESS
func handleLineItemEnriched(ctx context.Context, dapr daprd.Client, s *stats, cfg finops.DetectorConfig, baselineDays int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.Received.Add(1)
		w.Header().Set("Content-Type", "application/json")

		var evt cloudEvent
		if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
			log.Printf("bad envelope: %v", err)
			s.BadEnv.Add(1)
			_, _ = w.Write(drop)
			return
		}

		var item finops.EnrichedLineItem
		if err := json.Unmarshal(evt.Data, &item); err != nil {
			log.Printf("bad payload for evt %s: %v", evt.ID, err)
			s.BadEnv.Add(1)
			_, _ = w.Write(drop)
			return
		}

		dup, err := markProcessed(ctx, dapr, item.ID)
		if err != nil {
			log.Printf("processed-mark error for %s: %v", item.ID, err)
			s.Failed.Add(1)
			_, _ = w.Write(retry)
			return
		}
		if dup {
			s.Duplicate.Add(1)
			_, _ = w.Write(success)
			return
		}

		if err := applyRollup(ctx, dapr, item); err != nil {
			log.Printf("apply rollup for %s: %v", item.ID, err)
			s.Failed.Add(1)
			_, _ = w.Write(retry)
			return
		}
		s.Applied.Add(1)

		// Event-driven anomaly detection: after every rollup update, check
		// whether the current (team, service, day) trips the threshold vs
		// its trailing baseline. Best-effort — if detection fails we've
		// still ACKed the rollup work.
		if err := runDetection(ctx, dapr, item.TeamID, item.Service, item.Day, cfg, baselineDays, s); err != nil {
			log.Printf("detection for %s/%s/%s: %v", item.TeamID, item.Service, item.Day, err)
		}

		_, _ = w.Write(success)
	}
}

// handleDetectBatch scans all known (team, service) rollups for a given day
// and runs anomaly detection on each. Idempotent — repeated calls do not
// publish duplicate anomaly events (marker in state-postgres). Useful for
// backtesting, `make anomaly-demo`, and deterministic verification.
//
//   POST /detect?day=YYYY-MM-DD
func handleDetectBatch(ctx context.Context, dapr daprd.Client, s *stats, cfg finops.DetectorConfig, baselineDays int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		day := r.URL.Query().Get("day")
		if day == "" {
			day = time.Now().UTC().Format("2006-01-02")
		}
		if _, err := time.Parse("2006-01-02", day); err != nil {
			http.Error(w, "day must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}

		centers, err := listCostCenters(ctx, dapr)
		if err != nil {
			http.Error(w, fmt.Sprintf("list cost centers: %v", err), http.StatusBadGateway)
			return
		}

		scanned, hits, dups := 0, 0, 0
		for _, cc := range centers {
			for _, svc := range knownServices {
				scanned++
				before := s.Detected.Load()
				beforeDup := s.DetectedDup.Load()
				if err := runDetection(ctx, dapr, cc.TeamID, svc, day, cfg, baselineDays, s); err != nil {
					log.Printf("batch detect %s/%s/%s: %v", cc.TeamID, svc, day, err)
					continue
				}
				if s.Detected.Load() > before {
					hits++
				} else if s.DetectedDup.Load() > beforeDup {
					dups++
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"day":       day,
			"scanned":   scanned,
			"detected":  hits,
			"duplicate": dups,
		})
	}
}

// runDetection reads the current-day rollup for (team, service), fetches the
// prior N days as history, and if Detect returns an anomaly it persists a
// marker (FirstWrite for idempotency) and publishes to `anomaly.detected`.
// No-ops if the current-day rollup is absent (no data → nothing to detect).
func runDetection(ctx context.Context, dapr daprd.Client, teamID, service, day string, cfg finops.DetectorConfig, baselineDays int, s *stats) error {
	current, ok, err := fetchRollup(ctx, dapr, day, teamID, service)
	if err != nil {
		return fmt.Errorf("fetch current: %w", err)
	}
	if !ok {
		return nil
	}

	history, err := fetchHistory(ctx, dapr, day, teamID, service, baselineDays)
	if err != nil {
		return fmt.Errorf("fetch history: %w", err)
	}

	anomaly := finops.Detect(current, history, cfg, time.Now())
	if anomaly == nil {
		return nil
	}

	// Idempotency: FirstWrite on `anomaly:<day>:<team>:<service>` so re-detecting
	// the same anomaly (event-driven update after batch detect, or vice-versa)
	// doesn't publish duplicate events.
	body, _ := json.Marshal(anomaly)
	err = dapr.SaveState(ctx, stateStore, anomaly.ID(), body, nil,
		daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite),
		daprd.WithConsistency(daprd.StateConsistencyStrong),
	)
	if err != nil {
		if isConcurrencyConflict(err) {
			s.DetectedDup.Add(1)
			return nil
		}
		return fmt.Errorf("save anomaly marker: %w", err)
	}

	if err := dapr.PublishEvent(ctx, pubsubName, anomalyTopic, anomaly); err != nil {
		return fmt.Errorf("publish anomaly: %w", err)
	}
	s.Detected.Add(1)
	log.Printf("ANOMALY: %s cost=$%.2f baseline=$%.2f (+%.0f%%)",
		anomaly.ID(), anomaly.ActualCostUSD, anomaly.BaselineCostUSD, anomaly.DeltaPct)
	return nil
}

// fetchRollup gets a specific rollup by (day, team, service). Returns
// (rollup, false, nil) if the key doesn't exist.
func fetchRollup(ctx context.Context, dapr daprd.Client, day, teamID, service string) (finops.Rollup, bool, error) {
	key := fmt.Sprintf("rollup:%s:%s:%s", day, teamID, service)
	got, err := dapr.GetState(ctx, stateStore, key, nil)
	if err != nil {
		return finops.Rollup{}, false, err
	}
	if got == nil || len(got.Value) == 0 {
		return finops.Rollup{}, false, nil
	}
	var r finops.Rollup
	if err := json.Unmarshal(got.Value, &r); err != nil {
		return finops.Rollup{}, false, err
	}
	return r, true, nil
}

// fetchHistory returns up to `n` prior-day rollups for the (team, service).
// Missing days are silently skipped — the caller (Detect) handles empty
// history correctly.
func fetchHistory(ctx context.Context, dapr daprd.Client, endDay, teamID, service string, n int) ([]finops.Rollup, error) {
	end, err := time.Parse("2006-01-02", endDay)
	if err != nil {
		return nil, err
	}
	out := make([]finops.Rollup, 0, n)
	for i := 1; i <= n; i++ {
		d := end.AddDate(0, 0, -i).Format("2006-01-02")
		r, ok, err := fetchRollup(ctx, dapr, d, teamID, service)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// listCostCenters returns the set of known cost centers so batch detection
// can iterate the (team, service) cross-product for a given day.
//
// We read from the shared ConfigMap mounted at /seed rather than from the
// Dapr state store, because Dapr auto-prefixes state keys with the calling
// app-id — ingest-svc's `cost-center:*` keys live under `ingest-svc||` in
// Redis and are invisible to rollup-svc. Bypassing that isolation would
// require setting `keyPrefix: none` on the state-redis Component, which
// would break the multi-app safety we care about elsewhere. Sharing the
// reference data via a ConfigMap that both pods mount is the honest fix.
// See NOTES.md T9 for the full discussion.
func listCostCenters(_ context.Context, _ daprd.Client) ([]finops.CostCenterInfo, error) {
	f, err := os.Open(seedFilePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", seedFilePath, err)
	}
	defer f.Close()
	var out []finops.CostCenterInfo
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", seedFilePath, err)
	}
	return out, nil
}

// markProcessed tries to insert "processed:<id>" with FirstWrite concurrency.
// Returns (dup=true, err=nil) if the key already existed.
func markProcessed(ctx context.Context, dapr daprd.Client, id string) (bool, error) {
	key := "processed:" + id
	err := dapr.SaveState(ctx, stateStore, key, []byte("1"), nil,
		daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite),
		daprd.WithConsistency(daprd.StateConsistencyStrong),
	)
	if err == nil {
		return false, nil
	}
	if isConcurrencyConflict(err) {
		return true, nil
	}
	return false, err
}

// applyRollup upserts the rollup for this line item using ETag-based
// optimistic concurrency to survive races with other subscriber replicas or
// parallel dispatches.
func applyRollup(ctx context.Context, dapr daprd.Client, item finops.EnrichedLineItem) error {
	delta := finops.FromLineItem(item, time.Now())
	key := delta.Key()

	for attempt := 1; attempt <= maxETagRetries; attempt++ {
		got, err := dapr.GetState(ctx, stateStore, key, nil)
		if err != nil {
			return err
		}

		var base finops.Rollup
		if got != nil && len(got.Value) > 0 {
			if err := json.Unmarshal(got.Value, &base); err != nil {
				return err
			}
		}

		merged := base.Merge(delta)
		body, err := json.Marshal(merged)
		if err != nil {
			return err
		}

		// If we read a value with an etag, only overwrite if it hasn't changed
		// under us. If the key didn't exist, SaveState with FirstWrite ensures
		// nobody else raced us to create it.
		if got != nil && got.Etag != "" {
			err = dapr.SaveStateWithETag(ctx, stateStore, key, body, got.Etag, nil,
				daprd.WithConcurrency(daprd.StateConcurrencyLastWrite),
				daprd.WithConsistency(daprd.StateConsistencyStrong),
			)
		} else {
			err = dapr.SaveState(ctx, stateStore, key, body, nil,
				daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite),
				daprd.WithConsistency(daprd.StateConsistencyStrong),
			)
		}
		if err == nil {
			return nil
		}
		if !isConcurrencyConflict(err) {
			return err
		}
		// Backoff a hair; another goroutine won the race, re-read and re-merge.
		time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)
	}
	return errors.New("etag retries exhausted")
}

// isConcurrencyConflict detects Dapr's ETag / FirstWrite conflict errors.
// The wire text varies by state store implementation:
//   - postgres v2: "no item was updated" (INSERT ON CONFLICT DO NOTHING → 0 rows affected)
//   - redis:       "possible etag mismatch"
//   - generic:     "etag mismatch", "already exists", "duplicate key"
func isConcurrencyConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "etag") ||
		strings.Contains(msg, "possible etag mismatch") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "no item was updated")
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
