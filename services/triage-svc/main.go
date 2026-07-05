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

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/workflow"
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
	mux.HandleFunc("/events/anomaly-detected", handleAnomalyDetected(ctx, wfClient, s))
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
func handleAnomalyDetected(
	ctx context.Context,
	wfClient *workflow.Client,
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
