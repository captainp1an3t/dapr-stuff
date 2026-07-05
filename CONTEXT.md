# Context

## Purpose

This repository is a **learning vehicle for Dapr**. The application it hosts is a means, not an end. Design decisions optimise for _surface area of Dapr features exercised_ over business realism. The primary deliverable is a **narrative** — a set of observations about where Dapr helps and where it hurts — captured in `NOTES.md` as we build.

## Domain

The application is a deliberately shallow **FinOps** slice: ingest synthetic cloud billing data, enrich it with ownership metadata, aggregate it, detect anomalies, and route those anomalies to humans for triage. FinOps is the vehicle; nothing in this repo is intended to be a real FinOps platform.

## Phases

The work is organised into three phases. Each phase produces a self-contained narrative arc that the eventual demo can lean on.

- **Phase 1 — Dapr solo, core building blocks.** The core five (service invocation, state, pub/sub, secrets, workflows) end-to-end with a full observability stack. Establishes the baseline "what Dapr does and what it costs". See `tasks/dapr-finops-v1.md`.
- **Phase 2 — Dapr solo, deeper.** Actors and bindings. Pushes the model into its more opinionated corners. Task file created when Phase 1 lands.
- **Phase 3 — Dapr + Istio ambient mesh coexistence.** Introduce a service mesh and demonstrate the **handoff** — which Dapr features migrate to the mesh (mTLS, retries, tracing overlap), which stay 100% owned by Dapr (state, pub/sub, secrets, workflows, actors, bindings), and which become architectural trade-offs. This is the phase that turns the pitch from "Dapr is great" into "here is how Dapr fits into a mesh-shop like ours". Task file created when Phase 2 lands.

## Glossary

- **Line item** — a single row from a (synthetic) cloud billing extract: service, quantity, cost, tags, timestamp.
- **Enriched line item** — a line item after ownership lookup has attached cost center and owning team.
- **Cost center** — the financial allocation unit a line item belongs to, derived from a tag on the line item.
- **Team** — the owning group for one or more cost centers. Teams are the recipient of notifications.
- **Rollup** — an aggregation of enriched line items along one or more dimensions (team, service, day).
- **Baseline** — the rolling-window average of a rollup used as the comparison reference for anomaly detection.
- **Anomaly** — a rollup value that exceeds its baseline by a defined threshold. Emitted as a first-class event.
- **Triage** — the workflow of routing an anomaly to a human for acknowledgement or dismissal, with escalation on timeout.
- **Optimisation** — a proposed cost-reducing action (v1 bonus workflow; simulated in v1, not applied to real infrastructure).

_(Terms are refined here as they are sharpened during implementation.)_

## Scope

### Dapr building blocks — v1 (core five)

- **Service invocation** — service-to-service calls with mTLS, retries, tracing
- **State management** — key/value store abstraction
- **Pub/Sub** — message broker abstraction
- **Secrets** — secret store abstraction
- **Workflows** — durable, code-first orchestrations

### Dapr building blocks — v2 (stretch)

- **Actors** — virtual actor model for stateful units
- **Bindings** — input/output triggers to external systems

### Explicitly out of scope (for now)

- Configuration, Distributed lock, Cryptography, Conversation (LLM abstraction)
- Real cloud provider integration — everything is simulated so the demo runs offline in KinD
- Multi-cloud normalisation
- Realistic FinOps depth (amortisation, RI/SP coverage, unit economics)
- Frontend richness beyond Grafana dashboards + a tiny HTMX page for human-in-the-loop actions

### v1 domain slice

1. Synthetic billing extract dropped as a file (v1: HTTP POST; v2: binding trigger)
2. Ingest → normalise → enrich pipeline via service invocation and pub/sub
3. Rollups per team / service / day persisted for querying
4. Anomaly detection vs. rolling 7-day baseline; anomalies emitted as events
5. Grafana dashboards querying the rollup store directly
6. **Anomaly triage workflow** — event → notify owning team → wait for acknowledgement or auto-escalate
7. **Optimisation approval workflow** (bonus) — simulated "idle resources" → approve/reject → record decision
