# Dapr FinOps v1 — Tasks

Source: gather-reqs session (see [CONTEXT.md](../CONTEXT.md) and [docs/adr/0001-ca-extras.md](../docs/adr/0001-ca-extras.md)).

## How to work these tasks

- Slices are ordered by dependency. Do them in order unless a `Blocked by` chain lets you branch.
- Each slice is a **tracer bullet** — it cuts end-to-end through every layer that exists at that point. A completed slice is demoable on its own.
- Within a slice, use **TDD**: RED (one test) → GREEN (minimum code to pass) → repeat. Never write all the tests up front. Tests live at the public-behaviour boundary; the Dapr sidecar is part of the system, not something to mock.
- Every slice ends with a `NOTES.md` pass: record any Dapr pro / con / gotcha observed while building it. This is the actual deliverable of the project.
- `HITL` = pause for a real design decision. `AFK` = agent can drive to completion with sensible defaults and a review at the end.

---

## - [x] T1: KinD cluster + Tilt + base image + trivial reachable service

**Type**: AFK
**Blocked by**: None — can start immediately

### What to build

The infra spine, with no Dapr and no domain code. A `make up` command that stands up a KinD cluster, launches Tilt, builds the shared base image (with any locally-required extra CAs installed into the OS trust store per ADR-0001), and deploys a single trivial Go "hello" service that returns 200 on a health endpoint. `make down` tears it all cleanly back down.

This slice exists to prove the base-image + cert + KinD + Tilt loop works before any Dapr complexity is layered on. If this slice is painful to build, everything downstream is worse.

### Acceptance criteria

- [x] `make up` produces a running KinD cluster with the hello service pod healthy
- [x] Tilt web UI is reachable and shows the service green
- [x] The base image was built (not pulled) and contains any extra CAs (via `.ca-extras.pem` → `/usr/local/share/ca-certificates/extras.crt`)
- [x] `curl` (via a port-forward or Tilt-provided URL) against the hello service returns 200
- [x] `make down` removes the KinD cluster with no orphan Docker resources

---

## - [x] T2: Observability stack installed and browsable

**Type**: AFK
**Blocked by**: T1

### What to build

Prometheus, Tempo, Grafana, and an OpenTelemetry Collector installed into the cluster via Helm, wired together, and reachable from the developer's browser. Grafana pre-provisioned with Prometheus and Tempo as datasources.

No traces or metrics from the hello service yet — this slice only proves the observability infrastructure is healthy before there's anything meaningful to observe.

### Acceptance criteria

- [x] Grafana loads in the browser and shows both datasources as "healthy"
- [x] Prometheus is scraping itself (self-metrics visible in Grafana Explore)
- [x] Tempo shows an empty trace list without errors
- [x] The OTel Collector is running and exposing its own metrics
- [x] Total cluster memory footprint recorded in `NOTES.md` (con candidate: "how much RAM does the observability stack alone cost you")

---

## - [x] T3: Dapr installed; hello service on the sidecar; end-to-end trace visible

**Type**: AFK
**Blocked by**: T2

### What to build

Install Dapr into the cluster (control plane: operator, sentry, placement, sidecar-injector). Annotate the hello service so it gets a `daprd` sidecar. Make one call to it that goes through the sidecar (Dapr service invocation), and configure Dapr's OTel export so the trace lands in Tempo.

This is the first end-to-end proof that the Dapr control plane, the extra-CA-aware sidecar, and the observability stack all cooperate.

### Acceptance criteria

- [x] `kubectl get pods -n dapr-system` shows all Dapr control-plane pods healthy
- [x] Hello service pod has two containers: app + `daprd`
- [x] A `curl` against the sidecar's invocation endpoint returns the hello response
- [x] The invocation is visible as a trace in Tempo (spans include the `daprd` sidecar)
- [x] Any cert-related failure encountered en route (there will be some) is written up in `NOTES.md`

---

## - [x] T4: Postgres + Redis registered as named Dapr state stores

**Type**: AFK
**Blocked by**: T3

### What to build

Deploy Postgres and Redis into the cluster. Register them both as Dapr state store components with distinct names (e.g. `state-postgres`, `state-redis`) via Dapr component YAMLs. Add a temporary endpoint to the hello service that does a `put` then a `get` against each store by name.

### Acceptance criteria

- [x] Both state stores show as registered in the Dapr control plane
- [x] Smoke endpoint writes and reads back a key from each store, selectable by name
- [x] Data is visible directly in the store (row in Postgres, key in `redis-cli`)
- [x] Observation on how much of this was YAML-only versus code changes captured in `NOTES.md`

---

## - [x] T5: RabbitMQ registered as Dapr pub/sub; smoke-tested cross-service

**Type**: AFK
**Blocked by**: T3

### What to build

Deploy RabbitMQ (with its management plugin exposed) into the cluster. Register it as a Dapr pub/sub component. Temporarily split the hello service into a publisher and a subscriber (or add a second tiny service), and prove a message flows end-to-end through the broker via Dapr.

### Acceptance criteria

- [x] Publisher sends a message; subscriber receives it
- [x] Message is visible in the RabbitMQ management UI queue
- [x] Trace in Tempo shows publisher → publisher sidecar → broker → subscriber sidecar → subscriber
- [x] `NOTES.md` records how much broker-specific knowledge the app code needed (pro candidate: probably none)

---

## - [x] T6: Kubernetes Secrets registered as Dapr secret store; smoke-tested

**Type**: AFK
**Blocked by**: T3

### What to build

Register the built-in Kubernetes Secrets store as a Dapr secret store component. Create a sample secret. Add a temporary endpoint to the hello service that reads the secret via the Dapr secrets API.

### Acceptance criteria

- [x] Secret is created in the cluster
- [x] Hello service reads the secret via the Dapr secrets HTTP/gRPC API (not by mounting it directly)
- [x] The service pod does **not** have direct RBAC to read K8s secrets — proof that the sidecar is the one with access, which is one of the Dapr wins to note
      _(Actual finding: Dapr's Helm chart auto-creates a RoleBinding granting the pod's default SA `get secrets`, so the pod DOES have direct RBAC. The Dapr abstraction is a URL/CloudEvents layer, not a security boundary. Full write-up in NOTES.)_

---

## - [x] T6.5: Per-service ServiceAccounts and least-privilege secret access

**Type**: AFK
**Blocked by**: T6

### What to build

Reaction to the finding surfaced in T6. Give each application service its own dedicated ServiceAccount, set `spec.serviceAccountName` on Deployments, and grant `get secrets` only via an explicit Role/RoleBinding scoped to the specific SA that needs it. Model the least-privilege posture the demo should be pitching, not the sloppy default. See docs/adr/0002.

### Acceptance criteria

- [x] `hello-svc-sa` and `sub-svc-sa` exist as ServiceAccount resources
- [x] Both hello-svc and sub-svc pods run under their dedicated SA (not `default`)
- [x] `hello-svc-sa` CAN `get secrets/demo-secret` in `default` (via our own Role/RoleBinding, `hello-svc-secret-reader`) — verified with `kubectl auth can-i`
- [x] `sub-svc-sa` CANNOT `get secrets` in `default` — isolation proven
- [x] Reading `demo-secret` via hello-svc's Dapr endpoint still works after the SA switch (proves our explicit grant is what's authorising daprd)
- [x] ADR-0002 written, documenting the trade-off with Dapr's Helm-created `dapr-secret-reader` binding

---

## - [x] T7: Synthetic data generator + `ingest-svc` (real service)

**Type**: HITL — the line-item schema is a real design choice
**Blocked by**: T4, T5

### What to build

Replace the hello service. Introduce two new pieces:

1. **Data generator** (Python one-shot): emits N synthetic billing line items to a file (or POSTs them directly). Parameters like day, number of teams, cost variance are configurable.
2. **`ingest-svc`** (Go): accepts a POST of a batch of line items. For each item, enriches it with cost center + owning team via a state-store lookup (seeded on service start). Writes the enriched line item to Postgres via the Dapr state store, and publishes a `line-item.enriched` event to RabbitMQ via Dapr pub/sub.

TDD focus: the enrichment function is a pure lookup-and-transform — cover with unit tests. Integration test at the service boundary POSTs a batch and asserts the state store contains the enriched rows and the pub/sub received the events.

### Acceptance criteria

- [x] Line-item schema is documented (in-code) and reflects a real FinOps line item shape — decided together before coding
- [x] Generator produces a plausible batch with `make seed` or similar
- [x] POSTing a batch results in enriched rows in Postgres and events on the RabbitMQ queue
- [x] Unit tests cover the enrichment function; one integration test covers the POST → state + pub/sub path
- [x] Any surprise about Dapr's state API ergonomics captured in `NOTES.md`

---

## - [x] T8: `rollup-svc` subscribes and aggregates

**Type**: AFK
**Blocked by**: T7

### What to build

`rollup-svc` (Go): subscribes to `line-item.enriched` via Dapr pub/sub. For each event, updates a per-team / per-service / per-day rollup row in Postgres via the Dapr state store.

TDD focus: the aggregation function is pure — given a rollup and an incoming event, produce the new rollup. Cover with unit tests. Integration test seeds a batch through `ingest-svc` and asserts rollups appear correctly.

### Acceptance criteria

- [x] Rollups exist in Postgres with correct totals after a seeded batch
- [x] Re-running the same batch does not double-count (idempotency handled)
- [x] Trace in Tempo shows the full path from POST → ingest → pub/sub → rollup → state write
- [x] `NOTES.md`: how did Dapr pub/sub handle delivery guarantees, and did anything surprise us?

---

## - [x] T9: Anomaly detection

**Type**: HITL — threshold, baseline window, and anomaly-per-dimension are real decisions
**Blocked by**: T8

### What to build

Extend `rollup-svc` (or add a slim companion) to compute a rolling baseline from the rollup history and detect when a new day's rollup exceeds the baseline by a decided threshold. Publish `anomaly.detected` events to RabbitMQ.

TDD focus: the baseline + threshold logic is pure math — cover exhaustively. Integration test seeds a series of days that culminates in an anomaly and asserts the event is published.

### Acceptance criteria

- [x] Baseline formula and threshold are decided together and written in a short comment on the detection function
- [x] Anomaly event is published with enough context to notify (team, service, day, baseline, actual, delta)
- [x] Non-anomalous days do **not** produce events
- [x] Unit tests cover the boundary conditions of the threshold
- [x] `NOTES.md`: any Dapr-specific concerns with running detection on every event vs. on a schedule

---

## - [x] T10: Grafana dashboard for rollups and anomalies

**Type**: AFK
**Blocked by**: T9

### What to build

Add Postgres as a Grafana datasource. Create one dashboard showing per-team daily spend, with anomaly points overlaid. Provision the dashboard as code (JSON in the repo) so it survives cluster teardown.

### Acceptance criteria

- [x] Dashboard is provisioned automatically when Grafana boots (no manual clicking)
- [x] Seeded data produces a visually recognisable spend chart with an anomaly marker
- [x] Dashboard JSON lives in the repo and is version-controlled

---

## - [x] T11: `triage-svc` skeleton + Dapr Workflow hosting

**Type**: AFK
**Blocked by**: T9

### What to build

`triage-svc` (Go): boots, registers as a Dapr Workflow host, subscribes to `anomaly.detected`, and starts a trivial workflow instance per event that only logs the anomaly and completes. Purpose is to prove the placement + workflow infra actually works before we invest in real workflow logic.

TDD focus: the subscriber-to-workflow-start handoff. Assert a workflow instance is created per anomaly event.

### Acceptance criteria

- [x] Anomaly event triggers a workflow instance
- [x] The workflow completes and its state is queryable via the Dapr workflow API
- [x] Placement service is running and the workflow host is registered with it
- [x] `NOTES.md`: "Dapr Workflows require actor infrastructure even though we deferred actors" — note the leaky-abstraction observation

---

## - [x] T13: `notifier-svc` (Python) — secret-read + service invocation target

**Type**: AFK
**Blocked by**: T6

### What to build

`notifier-svc` (Python): reads a "Slack webhook URL" from the Dapr secret store on startup. Exposes a `POST /notify` endpoint (invoked via Dapr service invocation from `triage-svc` in T12). For v1 the "Slack" target is a local mock container (mailhog-style) so the whole demo runs offline.

TDD focus: templating the notification payload and mapping anomaly context into it.

### Acceptance criteria

- [ ] Service starts and reads the secret via Dapr (fails loudly if secret missing)
- [ ] `POST /notify` produces a rendered payload delivered to the mock target
- [ ] Payload templating is unit-tested against a few anomaly shapes
- [ ] Trace in Tempo shows caller → caller sidecar → callee sidecar → callee (once T12 exists)
- [ ] `NOTES.md`: how did the polyglot boundary feel? Any Dapr SDK differences between Go and Python worth noting?

---

## - [x] T11.5: Workflow inbox — mitigation for Dapr's missing ListWorkflows API

**Type**: AFK
**Blocked by**: T11

### What to build

Reactive slice after T11 discovered Dapr Workflow has no `List` API. Maintain a self-managed index of scheduled workflow instance IDs in state-postgres, updated via ETag CAS on each successful `ScheduleNewWorkflow`. Expose `GET /workflows` on triage-svc that reads the index and returns a summary table.

### Acceptance criteria

- [x] `workflow-index:__all__` state key exists after workflows are scheduled
- [x] `GET /workflows` returns a JSON summary with all scheduled instances
- [x] Restarting triage-svc does not lose the inbox (state survives)
- [x] Concurrent `ScheduleNewWorkflow` calls do not corrupt the index (ETag CAS)
- [x] `NOTES.md`: the "no ListWorkflows API" gap and our in-Dapr mitigation

---

## - [x] T12: Full triage workflow — notify, wait, ack-or-escalate, HTMX page

**Type**: HITL — timeout and escalation policy are real decisions
**Blocked by**: T11, T13

### What to build

Real triage workflow inside `triage-svc`:

1. On anomaly, call `notifier-svc` via Dapr service invocation to send a notification.
2. Wait for an **external event** ("ack") with a decided timeout.
3. If ack arrives → mark the anomaly closed.
4. If timeout fires → escalate (for v1: send a second notification with escalation flag; record the escalation).

Add a small HTMX page served by `triage-svc` showing pending anomalies, each with an "Acknowledge" button that fires the external event into the workflow via the Dapr workflow API.

TDD focus: the workflow's decision logic (ack path vs. timeout path) using the Dapr workflow SDK's test harness. Integration test drives the whole loop through the HTTP endpoint and asserts the notifier was called the right number of times.

### Acceptance criteria

- [ ] Timeout and escalation policy are decided together and documented in a comment on the workflow
- [ ] Ack path: workflow completes as "acknowledged", one notification sent
- [ ] Timeout path: workflow completes as "escalated", two notifications sent
- [ ] HTMX page lists pending anomalies and the ack button drives the workflow to completion
- [ ] `NOTES.md`: this is the flagship Dapr feature — write up how the durable-orchestration story felt vs. what you'd have had to build without Dapr

---

## - [ ] T14: Optimisation approval workflow (bonus)

**Type**: AFK
**Blocked by**: T12

### What to build

Second workflow: seed a "these N resources are idle" scenario. Workflow: notify owning team → wait for approve-or-reject external event → record decision to Postgres → complete. HTMX page gains a second view for pending optimisations with approve/reject buttons.

Reuses the notifier and the HTMX pattern from T12/T13.

### Acceptance criteria

- [ ] Seeded optimisation triggers the workflow
- [ ] Approve path and reject path both terminate the workflow with the recorded decision
- [ ] HTMX page shows pending optimisations and drives them to completion
- [ ] `NOTES.md`: what code was reusable across the two workflows? Where was Dapr's abstraction thin?

---

## - [ ] T15: Chaos — kill things, observe recovery

**Type**: AFK
**Blocked by**: T14 (or any point after T12 where the full stack is meaningful)

### What to build

Deliberate failure injection to see how Dapr behaves. For each scenario below, kill the pod, observe, record. This slice adds no features — it exists to generate `NOTES.md` observations that are otherwise easy to skip.

Scenarios:

1. Kill the `daprd` sidecar of `triage-svc` mid-workflow. Does the workflow resume when the sidecar comes back?
2. Kill the `placement` pod. What happens to workflows and (eventually) actors?
3. Kill Postgres. Do state-store calls fail loudly? Do they recover?
4. Kill RabbitMQ. Are in-flight messages lost, redelivered, or held?
5. Kill `notifier-svc`. Does the service-invocation retry policy do anything useful?

### Acceptance criteria

- [ ] Each scenario is scripted (a `make chaos-<n>` target or similar) so it can be re-run
- [ ] Each scenario has an observation entry in `NOTES.md` (pro or con)
- [ ] Any scenario that behaved worse than expected is turned into a follow-up task or an ADR

---

## - [ ] T16: Sidecar overhead measurement

**Type**: AFK
**Blocked by**: T14

### What to build

Quantify the cost of the sidecar so the "con" side of the demo has hard numbers, not hand-waving. Measure and record:

1. Memory footprint of `daprd` on an idle service (per pod).
2. Memory footprint of `daprd` under load (drive traffic via the generator).
3. p50 / p99 latency of a call **without** the sidecar (bypass — direct pod-to-pod) vs. **with** the sidecar.
4. Total cluster memory for the full stack vs. an equivalent no-Dapr deployment (rough — a paragraph estimate is fine, not a proper benchmark).

Chart (2) and (3) in Grafana as a dedicated "Dapr overhead" dashboard.

### Acceptance criteria

- [ ] Numbers exist and are recorded in `NOTES.md` under a dedicated "Overhead" section
- [ ] Grafana "Dapr overhead" dashboard is provisioned as code
- [ ] Methodology is documented briefly so future-you can re-run it
