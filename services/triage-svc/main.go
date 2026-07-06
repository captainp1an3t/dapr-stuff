package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cmar82/dapr-stuff/services/shared/finops"
	daprd "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/workflow"
	"github.com/microsoft/durabletask-go/task"
)

const (
	stateStore              = "state-postgres"
	workflowIndexKey        = "workflow-index:__all__"
	workflowIndexMaxRetries = 12
	notifierAppID           = "notifier-svc"
	notifyMethod            = "notify"
	ackEventName            = "ack"
	decisionEventName       = "decision"
	defaultAckTimeoutSecs   = 30
	defaultMaxEscalations   = 2
	defaultDecisionTimeoutSecs = 30
	decisionStateKeyPrefix  = "optimisation-decision:"
)

// activityDaprClient is set in main() and used by activity functions, which
// have no way to receive dependencies except via the activity registration
// closure (which the go-sdk RegisterActivity does not support). Package-level
// state is the honest option — activities are short-lived and stateless
// otherwise. Kept package-private and initialised exactly once.
var activityDaprClient daprd.Client

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

// notifyInput is the JSON contract between the workflow (Go) and the
// notifier-svc endpoint (Python). Field names match what Python's
// build_slack_payload() expects. Deliberately identical to the manual demo
// payload used in `make verify` T13.
type notifyInput struct {
	Kind    string         `json:"kind"`    // "initial" | "escalation"
	Anomaly finops.Anomaly `json:"anomaly"`
}

// ackEvent is the payload raised into the workflow when a human clicks the
// ack button. Small on purpose — the interesting bit is *that* it arrived,
// not what's in it.
type ackEvent struct {
	AckedBy string `json:"acked_by"`
	Note    string `json:"note,omitempty"`
}

// TriageWorkflow — T12 flagship workflow.
//
// Shape:
//   1. NotifyOwnerActivity(kind=initial)
//   2. Loop up to MAX_ESCALATIONS + 1 rounds:
//        a. WaitForExternalEvent("ack", ACK_TIMEOUT)
//        b. if ack arrives → return acked
//        c. if timeout → NotifyOwnerActivity(kind=escalation), continue
//   3. If we exhaust escalations → return unacked
//
// DETERMINISM: no clocks, no I/O, no rand. All side effects live in the
// activity. The loop counter is derived from the workflow's own decision
// tree, so replay is deterministic — Dapr will re-run this function on
// restart and the durabletask engine short-circuits already-completed tasks.
func TriageWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var anomaly finops.Anomaly
	if err := ctx.GetInput(&anomaly); err != nil {
		return nil, err
	}

	timeout := ackTimeout()
	maxEsc := maxEscalations()

	log.Printf("workflow: triaging anomaly %s (timeout=%s, max_escalations=%d)",
		anomaly.ID(), timeout, maxEsc)

	// Round 0: initial notification.
	if err := ctx.CallActivity(NotifyOwnerActivity,
		workflow.ActivityInput(notifyInput{Kind: "initial", Anomaly: anomaly}),
	).Await(nil); err != nil {
		return nil, fmt.Errorf("initial notify failed: %w", err)
	}

	// Rounds 1..maxEsc+1: wait, then escalate if timed out. Last round waits
	// but does not escalate again — we give up after maxEsc *escalations*.
	for round := 1; round <= maxEsc+1; round++ {
		var ack ackEvent
		err := ctx.WaitForExternalEvent(ackEventName, timeout).Await(&ack)
		if err == nil {
			// Human clicked ack.
			return map[string]any{
				"anomaly_id":   anomaly.ID(),
				"status":       "acked",
				"acked_by":     ack.AckedBy,
				"note":         ack.Note,
				"escalations":  round - 1,
			}, nil
		}
		if !errors.Is(err, task.ErrTaskCanceled) {
			// Any non-timeout error is unexpected — bubble up so the
			// workflow lands in FAILED, not silently absorbed.
			return nil, fmt.Errorf("wait for ack failed: %w", err)
		}

		// Timeout. If we still have escalations left, escalate and loop.
		if round <= maxEsc {
			if err := ctx.CallActivity(NotifyOwnerActivity,
				workflow.ActivityInput(notifyInput{Kind: "escalation", Anomaly: anomaly}),
			).Await(nil); err != nil {
				return nil, fmt.Errorf("escalation %d failed: %w", round, err)
			}
		}
	}

	// Exhausted all rounds without an ack.
	return map[string]any{
		"anomaly_id":  anomaly.ID(),
		"status":      "unacked",
		"escalations": maxEsc,
	}, nil
}

// NotifyOwnerActivity invokes notifier-svc via Dapr service invocation.
// This is the polyglot bridge: Go workflow activity → Python HTTP handler,
// mTLS between sidecars, single trace in Tempo (validates T13's pitch).
func NotifyOwnerActivity(ctx workflow.ActivityContext) (any, error) {
	var in notifyInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}

	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	content := &daprd.DataContent{
		ContentType: "application/json",
		Data:        body,
	}

	// Note: activity Context() is short-lived and belongs to the current
	// activity execution. Dapr replays skip completed activities entirely,
	// so retries only re-run activities that failed — the sidecar mTLS
	// call happens per attempt.
	_, err = activityDaprClient.InvokeMethodWithContent(ctx.Context(),
		notifierAppID, notifyMethod, "POST", content)
	if err != nil {
		return nil, fmt.Errorf("invoke %s.%s: %w", notifierAppID, notifyMethod, err)
	}

	log.Printf("activity: notified owner anomaly=%s kind=%s", in.Anomaly.ID(), in.Kind)
	return map[string]string{"delivered": in.Kind}, nil
}

// ---- T14 optimisation workflow ---------------------------------------------
//
// Second workflow, deliberately different shape from Triage so we can honestly
// answer "what was reusable?". Same overall structure (schedule → notify →
// wait → terminate), different domain type (IdleResource), different external
// events (approve/reject instead of ack), different terminal states (approved
// / rejected / expired), and it *persists a decision record to state store*
// which the triage flow does not.

// notifyOptimisationInput mirrors notifyInput but carries an IdleResource
// and is discriminated at the notifier by payload key ("optimisation" vs
// "anomaly"). Two structs rather than one polymorphic one keeps each
// activity honest about what it expects.
type notifyOptimisationInput struct {
	Kind         string              `json:"kind"`
	Optimisation finops.IdleResource `json:"optimisation"`
}

// decisionEvent is the payload for the "decision" external event. The
// caller sets `Decision` to either "approve" or "reject" (typically via
// the /workflows/{id}/approve or .../reject HTTP routes, which inject the
// value server-side). Same shape either way; the workflow branches on the
// Decision field. One event is simpler than racing two — durabletask-go
// v0.5.0 has no WhenAny, so sequential Awaits would block full timeouts.
type decisionEvent struct {
	Decision  string `json:"decision"` // "approve" | "reject"
	DecidedBy string `json:"decided_by"`
	Note      string `json:"note,omitempty"`
}

// decisionRecord is what we persist to state-postgres so the decision
// survives workflow purge. Keyed by IdleResource.ID().
type decisionRecord struct {
	ResourceID      string  `json:"resource_id"`
	TeamID          string  `json:"team_id"`
	SuggestedAction string  `json:"suggested_action"`
	Decision        string  `json:"decision"` // "approved" | "rejected" | "expired"
	DecidedBy       string  `json:"decided_by"`
	Note            string  `json:"note,omitempty"`
	DecidedAt       string  `json:"decided_at"`
	MonthlyWasteUSD float64 `json:"monthly_waste_usd"`
}

// OptimisationWorkflow — T14 second workflow.
//
// Shape:
//   1. NotifyOptimisationActivity (kind=optimisation-request)
//   2. WaitForExternalEvent("decision", timeout) — one event, discriminated
//      by the Decision field. The two HTTP routes /approve and /reject
//      inject the value server-side so callers get the RESTful URL they
//      expect, but the workflow only waits on one thing.
//   3. On decision, RecordDecisionActivity persists the outcome to state.
//   4. On timeout, the outcome is "expired" (no destructive action —
//      conservative default).
//
// DELIBERATE choice: no escalation loop. Approve/reject is binary —
// nagging someone repeatedly to make a destructive-vs-safe decision
// doesn't scale as a UX. Contrast with TriageWorkflow which escalates.
func OptimisationWorkflow(ctx *workflow.WorkflowContext) (any, error) {
	var res finops.IdleResource
	if err := ctx.GetInput(&res); err != nil {
		return nil, err
	}

	timeout := decisionTimeout()
	log.Printf("workflow: optimisation %s (timeout=%s)", res.ID(), timeout)

	if err := ctx.CallActivity(NotifyOptimisationActivity,
		workflow.ActivityInput(notifyOptimisationInput{
			Kind: "optimisation-request", Optimisation: res,
		}),
	).Await(nil); err != nil {
		return nil, fmt.Errorf("optimisation notify failed: %w", err)
	}

	var evt decisionEvent
	err := ctx.WaitForExternalEvent(decisionEventName, timeout).Await(&evt)

	decision := decisionRecord{
		ResourceID:      res.ResourceID,
		TeamID:          res.TeamID,
		SuggestedAction: res.SuggestedAction,
		MonthlyWasteUSD: res.MonthlyWasteUSD,
		DecidedAt:       ctx.CurrentUTCDateTime().UTC().Format(time.RFC3339),
	}

	switch {
	case err == nil && evt.Decision == "approve":
		decision.Decision = "approved"
		decision.DecidedBy = evt.DecidedBy
		decision.Note = evt.Note
	case err == nil && evt.Decision == "reject":
		decision.Decision = "rejected"
		decision.DecidedBy = evt.DecidedBy
		decision.Note = evt.Note
	case errors.Is(err, task.ErrTaskCanceled):
		decision.Decision = "expired"
		decision.DecidedBy = "system"
		decision.Note = "no decision received within timeout"
	default:
		return nil, fmt.Errorf("wait for decision failed: %w (evt=%+v)", err, evt)
	}

	// Persist the decision record. Non-fatal if it fails — we still complete
	// the workflow (the outcome is in the workflow output either way).
	if err := ctx.CallActivity(RecordDecisionActivity,
		workflow.ActivityInput(decision),
	).Await(nil); err != nil {
		log.Printf("WARN: RecordDecisionActivity failed for %s: %v", res.ID(), err)
	}

	return decision, nil
}

// NotifyOptimisationActivity — same shape as NotifyOwnerActivity but with
// a different payload discriminator. The notifier-svc /notify endpoint
// dispatches on payload shape.
func NotifyOptimisationActivity(ctx workflow.ActivityContext) (any, error) {
	var in notifyOptimisationInput
	if err := ctx.GetInput(&in); err != nil {
		return nil, err
	}

	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	content := &daprd.DataContent{
		ContentType: "application/json",
		Data:        body,
	}

	_, err = activityDaprClient.InvokeMethodWithContent(ctx.Context(),
		notifierAppID, notifyMethod, "POST", content)
	if err != nil {
		return nil, fmt.Errorf("invoke %s.%s: %w", notifierAppID, notifyMethod, err)
	}

	log.Printf("activity: notified optimisation %s kind=%s", in.Optimisation.ID(), in.Kind)
	return map[string]string{"delivered": in.Kind}, nil
}

// RecordDecisionActivity writes the decision to state-postgres under a
// deterministic key so it can be queried after the workflow is purged.
// Idempotent: re-runs overwrite the same key with the same content.
func RecordDecisionActivity(ctx workflow.ActivityContext) (any, error) {
	var d decisionRecord
	if err := ctx.GetInput(&d); err != nil {
		return nil, err
	}

	body, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}

	key := decisionStateKeyPrefix + d.ResourceID
	if err := activityDaprClient.SaveState(ctx.Context(), stateStore, key, body, nil); err != nil {
		return nil, fmt.Errorf("save decision %s: %w", key, err)
	}

	log.Printf("activity: recorded decision %s = %s by %s", d.ResourceID, d.Decision, d.DecidedBy)
	return map[string]string{"recorded": d.Decision}, nil
}

func decisionTimeout() time.Duration {
	if v := os.Getenv("DECISION_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defaultDecisionTimeoutSecs) * time.Second
}

// ---- end T14 ---------------------------------------------------------------

func ackTimeout() time.Duration {
	if v := os.Getenv("ACK_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defaultAckTimeoutSecs) * time.Second
}

func maxEscalations() int {
	if v := os.Getenv("MAX_ESCALATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMaxEscalations
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
	if err := worker.RegisterWorkflow(OptimisationWorkflow); err != nil {
		log.Fatalf("register optimisation workflow: %v", err)
	}
	if err := worker.RegisterActivity(NotifyOwnerActivity); err != nil {
		log.Fatalf("register activity: %v", err)
	}
	if err := worker.RegisterActivity(NotifyOptimisationActivity); err != nil {
		log.Fatalf("register optimisation notify activity: %v", err)
	}
	if err := worker.RegisterActivity(RecordDecisionActivity); err != nil {
		log.Fatalf("register record-decision activity: %v", err)
	}
	if err := worker.Start(); err != nil {
		log.Fatalf("start worker: %v", err)
	}
	defer worker.Shutdown()
	log.Printf("workflow worker started; TriageWorkflow + OptimisationWorkflow + 3 activities registered")

	// --- Workflow client: schedules workflow instances.
	wfClient, err := workflow.NewClient()
	if err != nil {
		log.Fatalf("workflow client: %v", err)
	}

	// --- Plain Dapr client — used both for the workflow index (T11.5) and,
	// via the package-level activityDaprClient, for service invocation from
	// activities (T12 NotifyOwnerActivity → notifier-svc).
	dc, err := daprd.NewClient()
	if err != nil {
		log.Fatalf("dapr client: %v", err)
	}
	defer dc.Close()
	activityDaprClient = dc

	s := &stats{}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/stats", handleStats(s))
	mux.HandleFunc("/events/anomaly-detected", handleAnomalyDetected(ctx, wfClient, dc, s))
	mux.HandleFunc("/triage", handleTriageStart(ctx, wfClient, dc, s))
	mux.HandleFunc("/optimisation", handleOptimisationStart(ctx, wfClient, dc, s))
	mux.HandleFunc("/optimisations", handleOptimisationList(ctx, wfClient, dc))
	mux.HandleFunc("/workflows", handleWorkflowInbox(ctx, wfClient, dc))
	mux.HandleFunc("/workflows/", handleWorkflowRouter(ctx, wfClient))

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

// handleTriageStart — POST /triage kicks off a workflow manually. Body is
// the anomaly JSON. Response: {instance_id, status}. Used by verify and by
// any human wanting to test without publishing to the pubsub topic.
//
// Deterministic instance ID (same as the pubsub path) so re-POSTing the same
// anomaly is a no-op ("duplicate" counter increments, 200 returned). This is
// the same idempotency contract as the pubsub path — one anomaly, one
// workflow, no matter how it arrived.
func handleTriageStart(
	ctx context.Context,
	wfClient *workflow.Client,
	dc daprd.Client,
	s *stats,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.Received.Add(1)
		w.Header().Set("Content-Type", "application/json")

		var anomaly finops.Anomaly
		if err := json.NewDecoder(r.Body).Decode(&anomaly); err != nil {
			s.BadEnv.Add(1)
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
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
				_ = json.NewEncoder(w).Encode(map[string]any{
					"instance_id": instanceID,
					"status":      "duplicate",
				})
				return
			}
			s.Failed.Add(1)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.Started.Add(1)
		if err := appendToWorkflowIndex(ctx, dc, instanceID); err != nil {
			log.Printf("WARN: workflow-index update failed for %s: %v", instanceID, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"instance_id": instanceID,
			"status":      "started",
		})
	}
}

// handleOptimisationStart — POST /optimisation kicks off an OptimisationWorkflow.
// Same shape as handleTriageStart but takes an IdleResource. The two handlers
// are essentially clones; the amount of duplicated boilerplate is one of the
// honest observations in NOTES.md T14 — Dapr's abstraction is at the workflow
// level, not at the "kick off any workflow" level. A generic dispatcher would
// require reflection or code-gen.
func handleOptimisationStart(
	ctx context.Context,
	wfClient *workflow.Client,
	dc daprd.Client,
	s *stats,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.Received.Add(1)
		w.Header().Set("Content-Type", "application/json")

		var res finops.IdleResource
		if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
			s.BadEnv.Add(1)
			http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		instanceID := optimisationInstanceID(res)
		_, err := wfClient.ScheduleNewWorkflow(ctx, "OptimisationWorkflow",
			workflow.WithInstanceID(instanceID),
			workflow.WithInput(res),
		)
		if err != nil {
			if isDuplicateInstance(err) {
				s.Duplicate.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"instance_id": instanceID,
					"status":      "duplicate",
				})
				return
			}
			s.Failed.Add(1)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.Started.Add(1)
		if err := appendToWorkflowIndex(ctx, dc, instanceID); err != nil {
			log.Printf("WARN: workflow-index update failed for %s: %v", instanceID, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"instance_id": instanceID,
			"status":      "started",
		})
	}
}

// handleOptimisationList — GET /optimisations reads the workflow index,
// filters to OptimisationWorkflow instances, and returns a summary list
// including the recorded decision (if any) pulled from state-postgres.
// Same shape as handleWorkflowInbox but pre-filtered by workflow type.
func handleOptimisationList(ctx context.Context, wfClient *workflow.Client, dc daprd.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ids, _, err := readWorkflowIndex(ctx, dc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		type summary struct {
			InstanceID    string `json:"instance_id"`
			Status        int32  `json:"status"`
			StatusName    string `json:"status_name"`
			CreatedAt     string `json:"created_at"`
			LastUpdatedAt string `json:"last_updated_at"`
			ResourceID    string `json:"resource_id,omitempty"`
			TeamID        string `json:"team_id,omitempty"`
			Decision      string `json:"decision,omitempty"`
			DecidedBy     string `json:"decided_by,omitempty"`
		}

		out := make([]summary, 0)
		for _, id := range ids {
			// Cheap prefix filter first; avoids fetching TriageWorkflow metadata.
			if !strings.HasPrefix(id, "opt-") {
				continue
			}
			meta, err := wfClient.FetchWorkflowMetadata(ctx, id, workflow.WithFetchPayloads(true))
			if err != nil {
				continue
			}

			s := summary{
				InstanceID:    meta.InstanceID,
				Status:        int32(meta.RuntimeStatus),
				StatusName:    meta.RuntimeStatus.String(),
				CreatedAt:     meta.CreatedAt.Format(time.RFC3339),
				LastUpdatedAt: meta.LastUpdatedAt.Format(time.RFC3339),
			}
			// Best-effort resource_id + team from the input payload.
			if meta.SerializedInput != "" {
				var res finops.IdleResource
				if err := json.Unmarshal([]byte(meta.SerializedInput), &res); err == nil {
					s.ResourceID = res.ResourceID
					s.TeamID = res.TeamID
					// Best-effort decision pull from state store.
					if dec, ok := readDecision(ctx, dc, res.ResourceID); ok {
						s.Decision = dec.Decision
						s.DecidedBy = dec.DecidedBy
					}
				}
			}
			out = append(out, s)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":         len(out),
			"optimisations": out,
		})
	}
}

// readDecision is a best-effort lookup of the persisted decision record.
// Returns (record, true) on hit, (empty, false) on miss or error. Not
// on the workflow's critical path — used by the list view.
func readDecision(ctx context.Context, dc daprd.Client, resourceID string) (decisionRecord, bool) {
	item, err := dc.GetState(ctx, stateStore, decisionStateKeyPrefix+resourceID, nil)
	if err != nil || item == nil || len(item.Value) == 0 {
		return decisionRecord{}, false
	}
	var d decisionRecord
	if err := json.Unmarshal(item.Value, &d); err != nil {
		return decisionRecord{}, false
	}
	return d, true
}

// handleWorkflowRouter dispatches /workflows/{id}, /workflows/{id}/ack,
// /workflows/{id}/approve, /workflows/{id}/reject, and /workflows/{id}/page
// based on suffix. Kept in one handler because Go's stdlib mux doesn't do
// path parameters.
func handleWorkflowRouter(ctx context.Context, wfClient *workflow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/workflows/")
		if rest == "" {
			http.Error(w, "expected /workflows/{instance-id}[/ack|/approve|/reject|/page]", http.StatusBadRequest)
			return
		}
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		suffix := ""
		if len(parts) == 2 {
			suffix = parts[1]
		}

		switch suffix {
		case "":
			handleWorkflowQuery(ctx, wfClient, w, id)
		case "ack":
			handleWorkflowEvent(ctx, wfClient, w, r, id, ackEventName, nil)
		case "approve":
			handleWorkflowEvent(ctx, wfClient, w, r, id, decisionEventName, map[string]any{"decision": "approve"})
		case "reject":
			handleWorkflowEvent(ctx, wfClient, w, r, id, decisionEventName, map[string]any{"decision": "reject"})
		case "page":
			handleWorkflowPage(ctx, wfClient, w, r, id)
		default:
			http.NotFound(w, r)
		}
	}
}

// handleWorkflowQuery — GET /workflows/{instance-id} returns the workflow
// metadata so verify (and humans) can inspect state without knowing the
// Dapr workflow HTTP API paths.
func handleWorkflowQuery(ctx context.Context, wfClient *workflow.Client, w http.ResponseWriter, id string) {
	meta, err := wfClient.FetchWorkflowMetadata(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(meta)
}

// handleWorkflowEvent raises a named external event on the workflow
// (ack / decision). Body is any JSON object; it's passed through as the
// event payload, optionally with `inject` fields merged in so callers of
// /approve and /reject get the decision baked in server-side without
// having to trust the client.
//
// The workflow's Await unmarshals into the struct it expects (ackEvent
// for "ack", decisionEvent for "decision").
//
// Dapr's RaiseEvent is fire-and-forget from the caller's perspective —
// a 200 here means "the event was accepted by the workflow engine", not
// "the workflow has processed it". The workflow itself may take a moment
// to observe the event; verify sleeps briefly before re-checking status.
func handleWorkflowEvent(ctx context.Context, wfClient *workflow.Client, w http.ResponseWriter, r *http.Request, id, eventName string, inject map[string]any) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	for k, v := range inject {
		body[k] = v
	}

	if err := wfClient.RaiseEvent(ctx, id, eventName, workflow.WithEventPayload(body)); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"instance_id": id,
		"event":       eventName,
		"status":      "event-raised",
		"payload":     body,
	})
}

// handleWorkflowPage — GET /workflows/{id}/page serves a tiny HTMX ack UI.
// No auth (demo affordance, single-user port-forward). Renders the workflow
// metadata + input, plus (only while the workflow is RUNNING) the
// appropriate buttons for the workflow type — Acknowledge for
// TriageWorkflow, Approve/Reject for OptimisationWorkflow.
//
// This is the T14 reusability payoff: one page, one handler, two workflow
// types. The polymorphism is:
//   - subject block: anomaly <dl> vs optimisation <dl>
//   - action block: single ack button vs approve/reject pair
//   - outcome block: acked/unacked vs approved/rejected/expired
// All three vary by workflow name, which we read from meta.Name.
func handleWorkflowPage(ctx context.Context, wfClient *workflow.Client, w http.ResponseWriter, _ *http.Request, id string) {
	meta, err := wfClient.FetchWorkflowMetadata(ctx, id, workflow.WithFetchPayloads(true))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	subjectBlock, actionRunningBlock, outcomeBlock, title, h1 := renderByWorkflowType(meta, id)

	statusStr := meta.RuntimeStatus.String()
	statusClass := "status"
	switch statusStr {
	case "COMPLETED":
		statusClass = "status completed"
	case "FAILED", "TERMINATED", "CANCELED":
		statusClass = "status failed"
	}

	actionBlock := ""
	isRunning := statusStr == "RUNNING" || statusStr == "PENDING"
	if isRunning {
		actionBlock = actionRunningBlock
	} else {
		actionBlock = outcomeBlock
	}

	page := fmt.Sprintf(`<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <script src="https://unpkg.com/htmx.org@1.9.12"></script>
  <script>
    // Called by any decision button's hx-on::after-request. On success,
    // give the workflow ~1.5s to observe the event and complete, then
    // reload so the status bar + outcome block re-render.
    function ackDone(evt) {
      if (!evt.detail || !evt.detail.successful) return;
      var btn = evt.target;
      btn.disabled = true;
      btn.textContent = 'Recorded \u2014 completing workflow...';
      // Disable sibling buttons too so a fast user can't double-click.
      var sibs = btn.parentNode.querySelectorAll('button');
      for (var i = 0; i < sibs.length; i++) sibs[i].disabled = true;
      setTimeout(function () { window.location.reload(); }, 1500);
    }
  </script>
  <style>
    body { font-family: -apple-system, sans-serif; max-width: 40em; margin: 2em auto; padding: 0 1em; color: #1a202c; }
    dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.25em 1em; }
    dt { font-weight: bold; }
    button { padding: 0.75em 1.5em; font-size: 1em; color: white; border: 0; border-radius: 4px; cursor: pointer; margin-right: 0.5em; }
    button.primary { background: #2b6cb0; }
    button.primary:hover { background: #2c5282; }
    button.approve { background: #38a169; }
    button.approve:hover { background: #276749; }
    button.reject { background: #c53030; }
    button.reject:hover { background: #9b2c2c; }
    .status { padding: 0.5em 0.75em; border-radius: 4px; margin: 1em 0; background: #edf2f7; }
    .status.completed { background: #c6f6d5; }
    .status.failed { background: #fed7d7; }
    .outcome { padding: 0.75em; border-radius: 4px; background: #f7fafc; border-left: 4px solid #4a5568; }
    .outcome.acked, .outcome.approved { border-left-color: #38a169; background: #f0fff4; }
    .outcome.unacked, .outcome.expired { border-left-color: #dd6b20; background: #fffaf0; }
    .outcome.rejected { border-left-color: #c53030; background: #fff5f5; }
    .outcome dl { margin: 0.25em 0; }
    small { color: #4a5568; }
  </style>
</head>
<body>
  <h1>%s</h1>
  <p class="%s">Workflow <code>%s</code> (<code>%s</code>) — status: <strong>%s</strong></p>
  %s
  <div id="ack-outcome">%s
  </div>
</body>
</html>`,
		htmlEscape(title),
		htmlEscape(h1),
		statusClass, htmlEscape(id), htmlEscape(meta.Name), htmlEscape(statusStr),
		subjectBlock,
		actionBlock)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

// renderByWorkflowType returns the four HTML fragments that vary by workflow
// type: subject <dl>, action block when RUNNING, outcome block when terminal,
// page <title>, and page <h1>.
//
// Dispatched on meta.Name. Anything Dapr doesn't know about falls back to
// a generic rendering that at least shows the metadata.
func renderByWorkflowType(meta *workflow.Metadata, id string) (subject, runningAction, outcome, title, h1 string) {
	switch meta.Name {
	case "TriageWorkflow":
		return renderTriage(meta, id)
	case "OptimisationWorkflow":
		return renderOptimisation(meta, id)
	default:
		return renderGeneric(meta, id)
	}
}

func renderTriage(meta *workflow.Metadata, id string) (subject, runningAction, outcome, title, h1 string) {
	title = "triage-svc ack — " + id
	h1 = "Cost anomaly triage"

	if meta.SerializedInput != "" {
		var a finops.Anomaly
		if err := json.Unmarshal([]byte(meta.SerializedInput), &a); err == nil {
			subject = fmt.Sprintf(
				`<dl>
  <dt>Team</dt><dd>%s (%s)</dd>
  <dt>Service</dt><dd>%s</dd>
  <dt>Day</dt><dd>%s</dd>
  <dt>Actual</dt><dd>$%.2f</dd>
  <dt>Baseline</dt><dd>$%.2f</dd>
  <dt>Delta</dt><dd>+%.0f%%</dd>
</dl>`,
				htmlEscape(a.TeamName), htmlEscape(a.TeamID),
				htmlEscape(a.Service), htmlEscape(a.Day),
				a.ActualCostUSD, a.BaselineCostUSD, a.DeltaPct)
		}
	}

	runningAction = fmt.Sprintf(`
    <button class="primary"
            hx-post="/workflows/%s/ack"
            hx-headers='{"Content-Type":"application/json"}'
            hx-vals='{"acked_by":"demo-human","note":"acked from HTMX"}'
            hx-swap="none"
            hx-on::after-request="ackDone(event)">
      Acknowledge
    </button>
    <p><small>Clicking raises an <code>ack</code> event on the workflow. The workflow's <code>WaitForExternalEvent("ack", %s)</code> resolves and the run completes.
    Page will reload automatically to show the outcome.</small></p>`,
		htmlEscape(id), ackTimeout())

	outcome = renderTriageOutcome(meta)
	return
}

func renderOptimisation(meta *workflow.Metadata, id string) (subject, runningAction, outcome, title, h1 string) {
	title = "triage-svc optimisation — " + id
	h1 = "Cost optimisation approval"

	if meta.SerializedInput != "" {
		var r finops.IdleResource
		if err := json.Unmarshal([]byte(meta.SerializedInput), &r); err == nil {
			subject = fmt.Sprintf(
				`<dl>
  <dt>Team</dt><dd>%s (%s)</dd>
  <dt>Service</dt><dd>%s</dd>
  <dt>Resource</dt><dd>%s <code>%s</code></dd>
  <dt>Idle for</dt><dd>%d days</dd>
  <dt>Monthly waste</dt><dd>$%.2f</dd>
  <dt>Suggested action</dt><dd><strong>%s</strong></dd>
</dl>`,
				htmlEscape(r.TeamName), htmlEscape(r.TeamID),
				htmlEscape(r.Service),
				htmlEscape(r.ResourceType), htmlEscape(r.ResourceID),
				r.DaysIdle, r.MonthlyWasteUSD,
				htmlEscape(r.SuggestedAction))
		}
	}

	runningAction = fmt.Sprintf(`
    <button class="approve"
            hx-post="/workflows/%s/approve"
            hx-headers='{"Content-Type":"application/json"}'
            hx-vals='{"decided_by":"demo-human","note":"approved from HTMX"}'
            hx-swap="none"
            hx-on::after-request="ackDone(event)">
      Approve
    </button>
    <button class="reject"
            hx-post="/workflows/%s/reject"
            hx-headers='{"Content-Type":"application/json"}'
            hx-vals='{"decided_by":"demo-human","note":"rejected from HTMX"}'
            hx-swap="none"
            hx-on::after-request="ackDone(event)">
      Reject
    </button>
    <p><small>Approving raises an <code>approve</code> event; rejecting raises <code>reject</code>. Either resolves the workflow's decision race. If neither is clicked within <code>%s</code>, the workflow completes as <em>expired</em> (no destructive action taken).
    Page will reload after clicking.</small></p>`,
		htmlEscape(id), htmlEscape(id), decisionTimeout())

	outcome = renderOptimisationOutcome(meta)
	return
}

func renderGeneric(meta *workflow.Metadata, id string) (subject, runningAction, outcome, title, h1 string) {
	title = "workflow — " + id
	h1 = "Workflow: " + meta.Name
	subject = fmt.Sprintf(`<p><small>No specialised rendering for workflow type <code>%s</code>.</small></p>`, htmlEscape(meta.Name))
	runningAction = `<p>This workflow type has no interactive controls defined.</p>`
	outcome = fmt.Sprintf(`<div class="outcome"><small>Output:</small><br><code>%s</code></div>`, htmlEscape(meta.SerializedOutput))
	return
}

// renderTriageOutcome pulls the workflow's serialised output and formats a
// small summary block for TriageWorkflow (acked / unacked). Falls back to
// raw output for anything unexpected.
func renderTriageOutcome(meta *workflow.Metadata) string {
	if meta.SerializedOutput == "" {
		return `<p class="outcome">Workflow ended with no output.</p>`
	}
	var out struct {
		AnomalyID   string `json:"anomaly_id"`
		Status      string `json:"status"`
		AckedBy     string `json:"acked_by"`
		Note        string `json:"note"`
		Escalations int    `json:"escalations"`
	}
	if err := json.Unmarshal([]byte(meta.SerializedOutput), &out); err != nil {
		return fmt.Sprintf(`<p class="outcome"><small>Raw output:</small><br><code>%s</code></p>`,
			htmlEscape(meta.SerializedOutput))
	}

	switch out.Status {
	case "acked":
		note := ""
		if out.Note != "" {
			note = fmt.Sprintf(`<dt>Note</dt><dd>%s</dd>`, htmlEscape(out.Note))
		}
		return fmt.Sprintf(`<div class="outcome acked">
      <strong>Acknowledged.</strong>
      <dl>
        <dt>By</dt><dd>%s</dd>
        %s
        <dt>Escalations before ack</dt><dd>%d</dd>
      </dl>
    </div>`, htmlEscape(out.AckedBy), note, out.Escalations)
	case "unacked":
		return fmt.Sprintf(`<div class="outcome unacked">
      <strong>Acknowledgement window expired.</strong>
      <dl>
        <dt>Escalations sent</dt><dd>%d</dd>
        <dt>Outcome</dt><dd>All notifications were delivered; nobody acknowledged in time.</dd>
      </dl>
    </div>`, out.Escalations)
	default:
		return fmt.Sprintf(`<div class="outcome"><small>Outcome:</small><br><code>%s</code></div>`,
			htmlEscape(meta.SerializedOutput))
	}
}

// renderOptimisationOutcome mirrors renderTriageOutcome for OptimisationWorkflow.
// Three terminal states — approved / rejected / expired — each with distinct
// framing. Colouring drives the story: green for approved, red for rejected,
// amber for expired (no decision, no destructive action taken).
func renderOptimisationOutcome(meta *workflow.Metadata) string {
	if meta.SerializedOutput == "" {
		return `<p class="outcome">Workflow ended with no output.</p>`
	}
	var d decisionRecord
	if err := json.Unmarshal([]byte(meta.SerializedOutput), &d); err != nil {
		return fmt.Sprintf(`<p class="outcome"><small>Raw output:</small><br><code>%s</code></p>`,
			htmlEscape(meta.SerializedOutput))
	}

	noteRow := ""
	if d.Note != "" {
		noteRow = fmt.Sprintf(`<dt>Note</dt><dd>%s</dd>`, htmlEscape(d.Note))
	}

	switch d.Decision {
	case "approved":
		return fmt.Sprintf(`<div class="outcome approved">
      <strong>Approved.</strong>
      <dl>
        <dt>By</dt><dd>%s</dd>
        %s
        <dt>At</dt><dd>%s</dd>
        <dt>Estimated monthly savings</dt><dd>$%.2f</dd>
      </dl>
      <p><small>Decision recorded to state-postgres under <code>%s%s</code>.</small></p>
    </div>`, htmlEscape(d.DecidedBy), noteRow, htmlEscape(d.DecidedAt),
			d.MonthlyWasteUSD, decisionStateKeyPrefix, htmlEscape(d.ResourceID))
	case "rejected":
		return fmt.Sprintf(`<div class="outcome rejected">
      <strong>Rejected.</strong>
      <dl>
        <dt>By</dt><dd>%s</dd>
        %s
        <dt>At</dt><dd>%s</dd>
      </dl>
      <p><small>Owner declined the suggested action; no cleanup will be performed.</small></p>
    </div>`, htmlEscape(d.DecidedBy), noteRow, htmlEscape(d.DecidedAt))
	case "expired":
		return fmt.Sprintf(`<div class="outcome expired">
      <strong>Decision window expired.</strong>
      <dl>
        <dt>Outcome</dt><dd>No decision received within the window.</dd>
        <dt>Default</dt><dd>No destructive action taken (conservative default).</dd>
      </dl>
    </div>`)
	default:
		return fmt.Sprintf(`<div class="outcome"><small>Outcome:</small><br><code>%s</code></div>`,
			htmlEscape(meta.SerializedOutput))
	}
}

// htmlEscape is a minimal HTML-attribute-safe escaper for the tiny page.
// stdlib html/template would be overkill for one page; keep the surface tiny.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
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

// optimisationInstanceID does the same for T14's IdleResource. Same
// colon-substitution discipline; different prefix so the two workflow
// families are visually distinguishable in the inbox.
func optimisationInstanceID(r finops.IdleResource) string {
	return "opt-" + strings.ReplaceAll(r.ID(), ":", "-")
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
