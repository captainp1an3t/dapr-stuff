package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/workflow"
)

const (
	stateStore          = "state-postgres"
	workflowIndexKey    = "workflow-index:__all__"
	workflowIndexMaxRetries = 12
)

// The Dapr workflow SDK writes its state through the built-in "dapr" workflow
// component, backed by placement + actors + the actorStateStore. Nothing to
// register component-side; just start a worker and a client.

// stats — lightweight counters for /stats.
type stats struct {
	Received  atomic.Int64
	Started   atomic.Int64
	Duplicate atomic.Int64
	BadEnv    atomic.Int64
	Failed    atomic.Int64
}

// cloudEvent — minimal shape we need from the Dapr envelope.
type cloudEvent struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// subResponse — Dapr's expected subscriber response envelope.
type subResponse struct {
	Status string `json:"status"`
}

var (
	success = mustJSON(subResponse{Status: "SUCCESS"})
	retry   = mustJSON(subResponse{Status: "RETRY"})
	drop    = mustJSON(subResponse{Status: "DROP"})
)

// TriageWorkflow — the T11 trivial workflow. Reads the anomaly, logs it,
// sets a custom status, returns a small result map, done. T12 will grow this
// into the full notify → wait-for-ack → escalate loop.
//
// DETERMINISM: workflow functions must be deterministic. That's easy here
// (no time.Now, no I/O, no rand) — the log call is the only side effect,
// and Dapr's runtime tolerates logs. Real side effects belong in activities.
func TriageWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var anomaly finops.Anomaly
	if err := ctx.GetInput(&anomaly); err != nil {
		return nil, err
	}

	// A logger call from inside a workflow is fine — it's an implicit
	// side effect Dapr accepts. Anything with side-effects that must be
	// observable OUTSIDE the workflow's own state (a call to notifier-svc,
	// a state write, an HTTP call) has to go in an activity — that comes
	// in T12.
	log.Printf("workflow: triaging anomaly %s — %s/%s day=%s delta=%.0f%%",
		anomaly.ID(), anomaly.TeamID, anomaly.Service, anomaly.Day, anomaly.DeltaPct)

	// Return value gets serialised into the workflow's completed state and
	// is fetchable via the workflow API (see /workflows/{id} on this service).
	return map[string]any{
		"anomaly_id": anomaly.ID(),
		"team_id":    anomaly.TeamID,
		"service":    anomaly.Service,
		"day":        anomaly.Day,
		"delta_pct":  anomaly.DeltaPct,
		"status":     "logged",
	}, nil
}

func main() {
	ctx := context.Background()

	// --- Workflow worker: hosts the TriageWorkflow function.
	worker, err := workflow.NewWorker()
	if err != nil {
		log.Fatalf("workflow worker: %v", err)
	}
	if err := worker.RegisterWorkflow(TriageWorkflow); err != nil {
		log.Fatalf("register workflow: %v", err)
	}
	if err := worker.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer worker.Shutdown()
	log.Printf("workflow worker started, TriageWorkflow registered")

	// --- Workflow client: schedules workflow instances.
	wfClient, err := workflow.NewClient()
	if err != nil {
		log.Fatalf("workflow client: %v", err)
	}

	// --- Plain Dapr client — currently unused, but T12 will use it to
	// invoke notifier-svc from activities.
	dc, err := daprd.NewClient()
	if err != nil {
		log.Fatalf("dapr client: %v", err)
	}
	defer dc.Close()

	s := &stats{}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats(s))
	mux.HandleFunc("/events/anomaly-detected", handleAnomalyDetected(ctx, wfClient, dc, s))
	mux.HandleFunc("/workflows", handleWorkflowInbox(ctx, wfClient, dc))
	mux.HandleFunc("/workflows/", handleWorkflowQuery(ctx, wfClient))

	addr := ":" + envOr("PORT", "8080")
	log.Printf("triage-svc listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"okay","service":"triage-svc"}`))
}

func handleStats(s *stats) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"received":  s.Received.Load(),
			"started":   s.Started.Load(),
			"duplicate": s.Duplicate.Load(),
			"bad_env":   s.BadEnv.Load(),
			"failed":    s.Failed.Load(),
		})
	}
}

// handleAnomalyDetected receives CloudEvents from the anomaly.detected topic
// and schedules a workflow instance per anomaly with a DETERMINISTIC
// instance ID. Re-delivery of the same event → duplicate-instance error →
// counted as duplicate and ACKed.
//
// On successful schedule, also appends the instance ID to a self-managed
// workflow index (workflow-index:__all__ in state-postgres, ETag-CAS updated).
// Dapr provides no ListWorkflows API — this is the mitigation. See T11.5 NOTES.
func handleAnomalyDetected(
	ctx context.Context,
	wfClient *workflow.Client,
	dc daprd.Client,
	s *stats,
) http.HandlerFunc {
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

		var anomaly finops.Anomaly
		if err := json.Unmarshal(evt.Data, &anomaly); err != nil {
			log.Printf("bad payload evt=%s: %v", evt.ID, err)
			s.BadEnv.Add(1)
			_, _ = w.Write(drop)
			return
		}

		instanceID := workflowInstanceID(anomaly)

		_, err := wfClient.ScheduleNewWorkflow(ctx, "TriageWorkflow",
			workflow.WithInstanceID(instanceID),
			workflow.WithInput(anomaly),
		)
		if err != nil {
			if isDuplicateInstance(err) {
				s.Duplicate.Add(1)
				_, _ = w.Write(success)
				return
			}
			log.Printf("schedule workflow %s: %v", instanceID, err)
			s.Failed.Add(1)
			_, _ = w.Write(retry)
			return
		}

		s.Started.Add(1)
		log.Printf("scheduled workflow instance=%s for anomaly=%s", instanceID, anomaly.ID())

		// Best-effort inbox update. If it fails, the workflow still ran — just
		// won't show up in GET /workflows. Never RETRY the pubsub message on
		// index failure (would cause duplicate-schedule loops).
		if err := appendToWorkflowIndex(ctx, dc, instanceID); err != nil {
			log.Printf("WARN: workflow-index update failed for %s: %v", instanceID, err)
		}

		_, _ = w.Write(success)
	}
}

// handleWorkflowQuery — GET /workflows/{instance-id} returns the workflow
// metadata so verify (and humans) can inspect state without knowing the
// Dapr workflow HTTP API paths.
func handleWorkflowQuery(ctx context.Context, wfClient *workflow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/workflows/")
		if id == "" {
			http.Error(w, "expected /workflows/{instance-id}", http.StatusBadRequest)
			return
		}
		meta, err := wfClient.FetchWorkflowMetadata(ctx, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	}
}

// handleWorkflowInbox — GET /workflows returns a summary of every workflow
// instance we've scheduled. Reads the self-managed index and calls
// FetchWorkflowMetadata for each ID.
//
// Dapr has no built-in ListWorkflows API. Our workaround: on each successful
// schedule, append the instance ID to a single array key
// (`workflow-index:__all__`) via ETag CAS. Retrieval is one state.GET + N
// FetchWorkflowMetadata calls. Fine for demo/moderate scale; would want a
// different index shape (e.g., paginated + by-status) for very high volume.
func handleWorkflowInbox(ctx context.Context, wfClient *workflow.Client, dc daprd.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ids, _, err := readWorkflowIndex(ctx, dc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		type summary struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Status        int32  `json:"status"`
			StatusName    string `json:"status_name"`
			CreatedAt     string `json:"created_at"`
			LastUpdatedAt string `json:"last_updated_at"`
		}

		out := make([]summary, 0, len(ids))
		for _, id := range ids {
			meta, err := wfClient.FetchWorkflowMetadata(ctx, id)
			if err != nil {
				// Instance may have been purged; skip.
				continue
			}
			out = append(out, summary{
				ID:            meta.InstanceID,
				Name:          meta.Name,
				Status:        int32(meta.RuntimeStatus),
				StatusName:    meta.RuntimeStatus.String(),
				CreatedAt:     meta.CreatedAt.Format("2006-01-02T15:04:05Z"),
				LastUpdatedAt: meta.LastUpdatedAt.Format("2006-01-02T15:04:05Z"),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":     len(out),
			"workflows": out,
		})
	}
}

// readWorkflowIndex fetches the workflow index array and its current ETag.
// Missing key returns an empty slice and empty ETag (no error).
func readWorkflowIndex(ctx context.Context, dc daprd.Client) ([]string, string, error) {
	item, err := dc.GetState(ctx, stateStore, workflowIndexKey, nil)
	if err != nil {
		return nil, "", err
	}
	if item == nil || len(item.Value) == 0 {
		return []string{}, "", nil
	}
	var ids []string
	if err := json.Unmarshal(item.Value, &ids); err != nil {
		return nil, "", err
	}
	return ids, item.Etag, nil
}

// appendToWorkflowIndex adds instanceID to the master list via ETag CAS.
// No-op if the ID is already present. Retries on concurrent-update conflict.
func appendToWorkflowIndex(ctx context.Context, dc daprd.Client, instanceID string) error {
	for attempt := 1; attempt <= workflowIndexMaxRetries; attempt++ {
		ids, etag, err := readWorkflowIndex(ctx, dc)
		if err != nil {
			return err
		}
		for _, existing := range ids {
			if existing == instanceID {
				return nil // already indexed
			}
		}
		ids = append(ids, instanceID)
		body, err := json.Marshal(ids)
		if err != nil {
			return err
		}

		if etag != "" {
			err = dc.SaveStateWithETag(ctx, stateStore, workflowIndexKey, body, etag, nil,
				daprd.WithConcurrency(daprd.StateConcurrencyLastWrite),
				daprd.WithConsistency(daprd.StateConsistencyStrong),
			)
		} else {
			err = dc.SaveState(ctx, stateStore, workflowIndexKey, body, nil,
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
		time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
	}
	return errors.New("workflow-index ETag retries exhausted")
}

// isConcurrencyConflict detects Dapr's ETag / FirstWrite conflict errors.
// Backend-specific wire text — see rollup-svc for the same helper.
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

// workflowInstanceID produces a deterministic instance ID from an anomaly.
// Dapr instance IDs must match [a-zA-Z0-9_-]+ so we replace colons.
func workflowInstanceID(a finops.Anomaly) string {
	return "triage-" + strings.ReplaceAll(a.ID(), ":", "-")
}

// isDuplicateInstance detects Dapr's "instance already exists" error, which
// is what ScheduleNewWorkflow returns when the instance ID is reused. Wire
// text depends on SDK version.
func isDuplicateInstance(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "instance already") ||
		strings.Contains(msg, "duplicate")
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

// keep the linter happy if any of these end up unused during T11 vs T12 shape:
var (
	_ = io.EOF
	_ = errors.New
)
