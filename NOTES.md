# Notes

Running log of pros, cons, and gotchas observed while building. The point of this file is that when the project is "done", the demo script writes itself.

Format: one dated H2 per slice, with `Pros`, `Cons`, and `Gotchas` subsections. Only include the sections that have content.

---

## T1 — KinD + Tilt + base image + reachable hello-svc

_2026-07-03_

### Gotchas (pre-Dapr, but they will affect every Dapr slice)

- **`apk add ca-certificates` is a chicken-and-egg trap when the alpine repo is proxied through a TLS-intercepting middlebox.** Initially "improved" past a pattern that side-steps it and hit the trap. `apk add`'s alpine-repo fetch goes through the intercepting proxy — which needs the CA we're trying to install — so it fails with an opaque OpenSSL error. Alpine ships `/etc/ssl/certs/ca-certificates.crt` as part of its baselayout, so the working pattern is: `cat our.crt >> /etc/ssl/certs/ca-certificates.crt`, **do not** run `apk add ca-certificates`, **do not** run `update-ca-certificates`. Fix now baked into `base.Dockerfile`; ADR-0001 reflects the actual mechanic.

- **CA subject strings are inside the base64 body of the PEM**, not searchable as literal text. Initial verification `grep -q '<subject>' bundle.crt` silently failed-succeeded (returned exit 1 but the `&&` sequence made it look green in a bulk build). Alpine's minimal image also has no `openssl` CLI. The reliable test: byte-exact tail-compare (`tail -c $size bundle.crt | cmp - our.crt`) — works without extra tools and proves the append landed intact. Used in `make verify`.

### Meta

- No Dapr yet, so no Dapr observations. Baseline established for later comparison.

---

## T2 — Observability stack (Prom + Tempo + OTel + Grafana)

_2026-07-03_

### Gotchas

- **`kind load docker-image` is a losing bet for large Helm charts.** kube-prometheus-stack alone pulled from five different registries (`docker.io/grafana`, `quay.io/kiwigrid`, `quay.io/prometheus-operator`, `quay.io/prometheus`, `registry.k8s.io/kube-state-metrics`). Curating a preload list per chart is a treadmill. Reversed the ADR-0001 decision within the first slice that exercised it: now we install the extra CA into the KinD node's system trust store at cluster-create time and restart containerd, and everything Just Works from any registry. See revised ADR-0001. Time cost of the reversal: ~15 minutes.
- **`docker.io` gets proxied differently from `quay.io` on the local intercepting network.** Grafana's `docker.io/grafana/grafana` image pulled fine even before the CA was in the node, while every `quay.io/*` image failed with `x509: certificate signed by unknown authority`. Not investigating further — the node-trust fix covers both — but noting the asymmetry in case it bites again.
- **KinD ships without metrics-server.** `kubectl top` returns nothing. If we ever want in-cluster memory/CPU metrics for the app, we install metrics-server or rely on Prometheus. For now, `docker stats dapr-stuff-control-plane` is the source of truth for total footprint.
- **`grafana/tempo` Helm chart prints a deprecation warning.** The single-binary chart is being retired in favour of `tempo-distributed` or the Tempo operator. Not blocking; a follow-up task to migrate before this repo bit-rots.

### Pros

- **Grafana pre-provisioned datasources via values file** worked cleanly — Prometheus was already there from the chart, and adding Tempo was `additionalDataSources:` in `kube-prom-stack.yaml`. No manual clicking in the UI, survives cluster recreation.
- **OTel Collector as a fan-in shim in the middle** means the Dapr sidecars (T3+) can push OTLP without knowing anything about the Tempo/Prometheus endpoints. The Collector pipeline (`otlp → batch → tempo/prometheus`) is the swappable seam Dapr keeps hinting at, and we now have it before we need it.

### Overhead

- Cluster memory with hello-svc + Kubernetes control plane + kube-prometheus-stack + Tempo + OTel Collector: **~2.4 GiB RSS on the KinD node** (measured via `docker stats`). Baseline for the sidecar-overhead comparison later in T16.
- **Grafana v13 wants more RAM than I first budgeted.** Initial limit of 256 MiB → OOMKilled (exit 137) after ~15 minutes, triggered by bleve dashboard-index building. Raised to 512 MiB. Rule of thumb: default Grafana settings assume ~500 MiB of headroom even without meaningful load.

### Meta

- Still no Dapr. But this slice cemented the pattern that infra (Helm) lives outside Tilt (`make infra-install`) and Tilt owns app iteration + port-forwards. Rebuilding app code with Tilt does not churn Helm — that's the correct rhythm.
- **Some Go binaries ignore `SSL_CERT_FILE`.** Grafana's update-checker (`grafana.com/api/...`) failed with `x509: certificate signed by unknown authority` even though the pod's env has `SSL_CERT_FILE` pointing at the OS trust store which contains the extra CA. Grafana appears to roll its own HTTP client config. Not a blocker (update check is optional), but a real Dapr-adjacent warning: **don't assume `SSL_CERT_FILE` is enough** — components that make outbound HTTPS calls may need their own CA config. Watch for this when picking Dapr state stores / brokers / secret stores that phone home.

---

## T3 — Dapr installed, hello-svc on the sidecar, trace in Tempo

_2026-07-04_

### Pros (Dapr's genuine wins, first-hand)

- **Sidecar injection is 4 annotations and a pod restart.** No app code changes, no SDK integration, no Docker changes. The `hello` container is byte-identical to T1 — the sidecar just appears next to it. This is the "invisible integration" pitch actually being true.
- **Distributed tracing to Tempo via one Configuration CRD.** Zero code changes. Set `spec.tracing.otel.endpointAddress` → hit `/health` → trace appears in Tempo. When we get to real services calling each other in T5+, this same setup should give us cross-service traces "for free".
- **Dapr auto-creates a `<app-id>-dapr` service** for each app-id — you don't manage those manually. (Caveat: it's headless and on port 80, see gotchas below — the abstraction is real but the details bit us.)

### Cons / Gotchas

- **`dapr-scheduler-server` came up with 3 replicas despite `global.ha.enabled: false`.** It's a raft ensemble and needs an odd count ≥ 3 for its guarantees, but the "HA off" label is misleading — you don't get a single-replica Dapr control plane even when you ask for one. Adds ~200 MiB on its own.
- **`<app-id>-dapr` auto-service uses port 80 → 3500, and is headless.** I burned 20 minutes assuming it exposed :3500 directly and would load-balance across pods. It doesn't. For multi-replica apps this headless behaviour is going to matter later.
- **The sidecar's `:3500` HTTP API is same-pod only.** Curling `hello-svc-dapr:80/v1.0/invoke/...` from a random pod resolves (headless returns a pod IP) but connection is refused — daprd only accepts local traffic on that port. Cross-pod invocation goes through mTLS on `:50002`. The clean test is: `curl localhost:3500/v1.0/invoke/...` from the host via a `kubectl port-forward` to a specific pod. This is a real "docs make it look simpler than it is" moment.
- **Tilt's API-discovery cache traps.** Tilt caches available Kubernetes API resources at startup. If Dapr installs its CRDs after Tilt starts (the normal `make up` order), applying a `dapr.io/v1alpha1 Configuration` via `k8s_yaml` fails with `no matches for kind Configuration`. Fix: declare the kind explicitly with `k8s_kind('Configuration', api_version='dapr.io/v1alpha1')` in the Tiltfile. This will presumably repeat for every Dapr CRD kind we use (Component, Subscription, Resiliency…).
- **Tempo's HTTP query port is 3200, not 3100** (my memory was wrong). Fixed in `make verify`.
- **Grafana admin auth broke after the T2 memory bump.** After `grafana` container OOM-restart within the same pod, admin/admin returned 401 via API. Had to `grafana cli admin reset-admin-password`. Suggests **`emptyDir` volumes survive container-level restarts, not just pod-level ones** — the SQLite DB from the previous crashed container was reused. Unrelated to Dapr but bit us mid-slice.

### Overhead

- After adding Dapr: KinD node at **~3.0 GiB RSS** (up from 2.4 GiB post-T2). ~600 MiB for `operator + placement + scheduler×3 + sentry + sidecar-injector + 1 daprd sidecar`. Per-sidecar cost will matter more once we have 4 services in T7+ — logged for T16.

### Meta

- First slice with real Dapr surface. Two of the three cons above (headless service port, same-pod-only :3500) came from my mental model being wrong, not Dapr being buggy. Real "docs vs. reality" learning.

---

## Strategic framing — the three-phase demo arc

_2026-07-04, mid-T3_

Realised mid-way through T3 that the pitch this project is building toward has a natural three-phase structure. Named it explicitly in `CONTEXT.md`:

- **Phase 1 (v1)** — Dapr solo, core five. Baseline "what does Dapr do and cost".
- **Phase 2 (v2)** — Dapr solo, deeper. Actors + bindings.
- **Phase 3 (v3)** — Dapr + Istio ambient. Show the **handoff**.

The insight worth capturing: **the Dapr adoption pitch inside a mesh shop is not "Dapr solves everything" — it's a table.**

| Dapr surface | In a mesh world |
|---|---|
| Service invocation, mTLS, retries, tracing | Overlap with mesh — architectural trade-off |
| State, pub/sub, secrets, workflows, actors, bindings | No mesh equivalent — Dapr keeps 100% of its value |

Phase 3 is where that table is _shown_, not asserted. The Phase-1 and Phase-2 work produces the numbers (memory, latency, complexity) that Phase 3 can then diff against. So the discipline of recording overhead in every slice is directly instrumental to the Phase-3 story landing.

Provisional Phase-3 tasks (T17–T21): install ambient; hand off mTLS; hand off one invocation path; add a waypoint L7 policy vs. Dapr AccessControl; final overhead measurement with the mesh in the picture. Not written to the tasks file yet — those show up when Phase 2 lands.

---

## T4 — Postgres + Redis as named Dapr state stores

_2026-07-04_

### Pros

- **Two backends, one API, zero app-code differentiation.** Same `POST /v1.0/state/<name>` with the same body shape works against Postgres and Redis. Swapping backing tech is a Component YAML edit. The pitch is real: **the app doesn't know or care what's behind the state name.**
- **Automatic migrations.** Dapr postgres v2 creates the `state` table and `dapr_metadata` on first component load. No init containers, no bootstrap SQL. That's a real convenience.
- **Automatic app-id scoping.** Keys stored as `<app-id>||<user-key>` (e.g. `hello-svc||smoke`). Multi-tenant safety without the app doing anything. Nice by-default.
- **Data really is in the backend, plaintext.** `SELECT key FROM state;` returns `hello-svc||smoke`. `redis-cli KEYS '*'` returns the same. No opaque Dapr metadata layer standing in the way — you can debug with your normal DB tools.

### Cons / Gotchas

- **Cached migration state is a real footgun.** Dapr's postgres component runs its "create tables" migration once, on component load, and caches success. If the postgres pod restarts and loses its data (in our case: `emptyDir` volume), daprd will happily continue trying to `INSERT` into a `state` table that no longer exists, returning `ERR_STATE_SAVE / relation "state" does not exist`. Fix: restart the app pod so daprd re-inits and re-runs migrations. In production with a proper PVC this doesn't happen, but any dev workflow that recreates Postgres has to also bounce every Dapr app that uses it.
- **Tilt's object auto-grouping is opinionated.** I tried to group all Dapr components under a `dapr-components` catch-all Tilt resource via `objects=[...]`, but Tilt had already assigned them (`appconfig` to hello-svc because of the annotation reference; state components to hello-svc because they were in the same `k8s_yaml` list). The "Valid remaining fragments are: [empty]" error means "everything is already spoken for." Simplest fix: let Tilt auto-group — the state components show up as sub-objects of hello-svc in the UI, which is actually correct because hello-svc is the only consumer.
- **Bitnami's chart shift makes local demos annoying.** Deliberately used plain k8s manifests for Postgres + Redis to skip the Bitnami / OCI paywall drama. That worked fine (30 lines each), so we probably won't ever reach for a Helm chart for these.
- **Dapr GET on a non-existent key returns 204 No Content, not 404.** My handler almost mis-classified this as an error — had to add `resp.StatusCode != http.StatusNoContent` to the error check. Sensible behaviour but non-obvious.

### Overhead

- Adding two data services + two Dapr components: no measurable jump on the sidecar (still ~1 daprd). Postgres pod ~50 MiB, Redis pod ~5 MiB. Neither is Dapr overhead.

### Meta

- First slice where the app made real outbound Dapr calls (`localhost:3500/v1.0/state/...`). The trace shape is finally interesting — you can see the sidecar's `state/statestore/SET` and `.../GET` spans without any app-side instrumentation.
- Hello-svc is still bare stdlib. The plan to introduce the Dapr Go SDK in T7 (when we replace hello-svc with real services) still holds — the "invisible integration" property has held all the way through T4.

### Tracing detail worth remembering

Inspected a real state-GET span pulled straight from Tempo. Dapr's OTel emission has some quirks:

- **`scope.name = "dapr-diagnostics"`** on every sidecar-emitted span. This is the cleanest "was this span from daprd?" signal — no need to look for `dapr.*` attrs.
- **Dapr uses `db.*` OTel conventions but populates them with Dapr-level info, not driver-level info.** `db.system="state"` (not `"postgresql"`), `db.name="state-postgres"` (the component name, not the actual DB), `db.statement="GET /v1.0/state/state-postgres/mykey"` (the Dapr URL, not SQL). Completely consistent with the abstraction — you can't tell from the trace which backend served the call. Some will love that (backend-agnostic queries), some will hate it (harder to correlate with DB-level metrics).
- **Two small Dapr bugs spotted in the emitted attrs:** `db.connection_string` only contains `"state"` instead of the full DSN (looks like it took just the dbname), and `server.address` is `"["` — a parser mishap on the IPv6 brackets in the postgres host DSN. Both cosmetic. Worth flagging when we get to a Dapr version bump.

---

## T5 — RabbitMQ pub/sub, cross-service

_2026-07-04_

### Pros

- **Publish is one Dapr POST, subscribe is one CRD.** Publisher code says `POST /v1.0/publish/pubsub-rabbitmq/hello-events`. Subscriber "code" is a Subscription CRD (routes → `/events`) plus an ordinary HTTP handler. No broker library, no AMQP frame parsing, no exchange/queue/binding declaration. **Same pitch as state, delivered again.**
- **Dapr auto-provisions the queue and exchange.** RabbitMQ management shows queue `sub-svc-hello-events` (named `<app-id>-<topic>`). Zero rabbitmq-cli commands needed. If a second app subscribed to the same topic, it'd get its own queue — competing-consumer semantics per app-id, for free.
- **CloudEvents envelope out of the box.** The body arriving at `/events` is a JSON CloudEvent with `id`, `source=<app-id>`, `type=com.dapr.event.sent`, `datacontenttype`, `data`, etc. — normalised regardless of broker. Portable metadata that would take real work to standardise on a raw Kafka/AMQP client.
- **First real cross-service trace.** publisher → publisher-sidecar → rabbitmq → subscriber-sidecar → subscriber, all in one trace. This is the story the "invisible integration" pitch has been building to.

### Cons / Gotchas

- **Dapr's RabbitMQ pubsub component fails FATAL on init if the broker isn't reachable.** Sidecar exits, pod crashloops. On cluster cold boot the app pods restarted ~3 times each until RabbitMQ was accepting connections. Contrast: the state store components (postgres/redis) tolerate a not-yet-ready backend at init — they retry lazily. This inconsistency across components is annoying, and "some Dapr components crashloop your pods on data-service outages" is a real production concern. Workaround for cold boot: `initContainers` on the app pods that wait for the broker, or `resource_deps` in Tilt (which we didn't need because eventual convergence worked).
- **Subscription CRD is `dapr.io/v2alpha1`** — different group version from Component (`v1alpha1`). Missed it once, added `k8s_kind('Subscription', api_version='dapr.io/v2alpha1')` explicitly to the Tiltfile.
- **`scopes: [sub-svc]` on the Subscription is critical.** Without it, the subscription tries to deliver to every Dapr app in the namespace whose config allows it. Explicit scoping avoids accidental cross-subscription.
- **Delivery latency is not instantaneous.** Publish → deliver was 1s in the smoke test (round-trip through the broker + Dapr's internal batching). Fine, but worth knowing when we build the workflow trigger in T11 — "anomaly detected" → workflow start is at least a broker round-trip.

### Overhead

- One more app pod (sub-svc + daprd), one more data service (RabbitMQ). RabbitMQ idles at ~150 MiB. Cluster is now at ~3.6 GiB (up ~600 MiB from post-T4).

### Meta

- End of the "sidecar + one broker" story arc. From T7 the pub/sub gets used for domain events, not smoke messages.
- Both hello-svc and sub-svc are still bare stdlib — pub/sub with a Dapr subscription is a Subscription CRD + a plain HTTP handler on the app side. Nothing SDK-shaped yet.

### Aside — "do I need the Dapr SDK?"

There is a `github.com/dapr/go-sdk` (and equivalents for every mainstream language). The choice is not "SDK vs no Dapr" — it's "raw HTTP on :3500 vs SDK on gRPC :50001". Both hit the same daprd sidecar; the SDK is a thin typed wrapper.

**Raw HTTP is fine when:**
- You want to see what's on the wire (transparency for demos and learning)
- You only do a handful of Dapr operations per service
- You want the literal "zero Dapr imports in app code" property (rare requirement but real)

**Reach for the SDK when:**
- You're doing state/pubsub ops in a hot loop — the SDK's typed API is genuinely cleaner
- You need workflows or actors — those are code-first and **only available via SDK** (no HTTP-only equivalent for authoring workflow logic)
- You care about latency — gRPC on :50001 is measurably faster than HTTP on :3500 for high-frequency ops
- You want built-in retry/backoff helpers instead of hand-rolling them

**What we'll do:** hello-svc and sub-svc (T3–T6) stay raw HTTP because they're throwaway smoke rigs. Every real service from T7 onward uses the SDK because workflows/actors need it and the ergonomics justify the one extra dependency.

---

## T6 — Kubernetes Secrets as a Dapr secret store

_2026-07-04_

### Pros

- **One-line Component + one endpoint.** `type: secretstores.kubernetes` with no metadata at all, then `GET /v1.0/secrets/secretstore-kubernetes/demo-secret` returns `{"key": "value", ...}` for every data key in the Secret. Textbook Dapr abstraction.
- **Same URL shape works for Vault later.** Swap the Component to `type: secretstores.hashicorp.vault` with appropriate metadata, keep the URL/code identical. This is the T-flip we'll do in v2 to make the "swap the secret store via YAML" pitch concrete.

### Cons / Gotchas (the big one)

- **The Dapr Helm chart auto-creates RBAC that broadens your default SA's powers.** On install, Dapr creates in every namespace it operates in:
  - `Role/secret-reader` — `get secrets` on `""` API group
  - `RoleBinding/dapr-secret-reader` — binds the `default` ServiceAccount to `secret-reader`

  So installing Dapr silently grants **every pod running as the default SA** the ability to `get` secrets — directly via the k8s API, not just through Dapr's abstraction. In a demo cluster that's convenient; in a production cluster with sensitive secrets in `default` namespace, it's a real posture change. **Security review must catch this.** Options if you don't want it:
  1. Uninstall the Helm-created RoleBinding after install and let pods use their own explicitly-created SAs with least-privilege
  2. Use a non-`default` SA for every app so the auto-grant doesn't apply
  3. Point Dapr at a secret store that isn't Kubernetes (Vault, cloud KMS) — then Dapr doesn't grant `get secrets` at all
- **My original mental model was wrong.** I assumed Dapr's sidecar had *separate* privileges from the app pod. It doesn't — the sidecar runs with the app pod's SA, and the Helm chart just makes sure that SA has enough permission. This is a good example of Dapr's abstraction leaking a real k8s security decision that the operator has to own.
- **`kubectl auth can-i --as=system:serviceaccount:...` is your friend.** For any Dapr install, run it against your app SAs to see what Dapr granted you didn't ask for.
- **CloudEvents envelope: not present for secrets.** State ops and pub/sub events get wrapped, secrets do not — you get a bare JSON object of `{key: value, ...}`. Consistent with the surface being read-only lookup, but inconsistent enough to note.

### Overhead

- Zero — just a Component + Secret + RoleBinding. No new pods.

### Meta

- End of infrastructure component slices (T4/T5/T6). Every core-five backend is now wired. Cluster memory unchanged from T5.
- **The pattern established:** Dapr components are almost always `(Component YAML with backend metadata) + (one endpoint on localhost:3500)`. Whether it's state, pub/sub, secrets, or later bindings/workflows, the shape is the same. That repeatability *is* the pitch.
- Hello-svc has now exercised: **service invocation, state (×2 backends), pub/sub, secrets.** Four building blocks. T7 kicks off the domain services and switches to the Dapr Go SDK.

---

## T6.5 — Per-service ServiceAccounts and least-privilege secret access

_2026-07-04_

Reactive slice to the T6 finding. Full rationale in [docs/adr/0002](docs/adr/0002-per-service-service-accounts.md).

### Pros

- **The mitigation is genuinely 6 lines of YAML per service** (SA + RoleBinding + `spec.serviceAccountName`). The demo now models the correct posture, and the pattern is directly copy-pasteable for real production services.
- **`kubectl auth can-i --as=...` gives a crisp go/no-go for every SA** — `hello-svc-sa` yes, `sub-svc-sa` no. That's the exact evidence a security review wants.
- **Dapr didn't fight the change.** daprd inherited the new SA cleanly, and secret reads kept working because our explicit RoleBinding covers the same permission. So the abstraction survives — you can adopt least-privilege without giving up any Dapr features.
- **`resourceNames: [demo-secret]` on the Role** — we scoped even the `get` verb to just the secret hello-svc legitimately needs. Even tighter than "get all secrets in namespace."

### Cons / Gotchas

- **We can't kill the Helm-created `dapr-secret-reader` RoleBinding** without overriding the Helm chart. It still binds `get secrets` to `default:default`. So the auto-grant is still there for any pod that runs as `default` — which shouldn't exist in production, but "shouldn't" is doing work. If security policy requires no unused RBAC, this is a Helm-chart-level fight for another day.
- **Every future service must remember to declare its own SA.** New-service checklist item. Easy to forget on the fifth Go service.
- **Cluster is now demonstrably safer than a `dapr init` install.** But someone doing a naive `dapr init` on their own cluster and running our workloads would silently pick up Dapr's default-SA grant. The mitigation is repo-scoped, not cluster-scoped.

### Meta

- This slice is the perfect kind of thing to have in a Dapr adoption pitch: "we found X, we recommend Y, here's the diff." It's the difference between "here are risks" and "here are risks with mitigations tested."

### Isolation proof — end-to-end

`kubectl auth can-i` alone is a strong RBAC-layer test, but we followed up with a live demo. From inside each pod, using the projected SA token, hit the k8s API for `demo-secret`:

```
sub-svc  pod (sub-svc-sa)   → HTTP 403 Forbidden
hello-svc pod (hello-svc-sa) → HTTP 200 with the Secret payload
```

Same base image, same TLS, same URL, same command — different results driven entirely by SA identity. **The API server rejects `sub-svc-sa` at the RBAC layer with no bypass available from inside the pod.** Whether the caller is app code, daprd, a sidecar we haven't installed yet, or a kubectl-exec'd wget, it all lands on the same 403.

---

## T7 — First real service (ingest-svc) with the Dapr Go SDK

_2026-07-04_

Hello-svc and sub-svc retired. `ingest-svc` (Go) + Python data generator arrive; the finops line-item schema locks in.

### Pros

- **Dapr Go SDK is a genuine ergonomics win over raw HTTP.** Three calls per line item — `SaveState`, `GetState`, `PublishEvent` — read like a normal Go API. Compare with the raw-HTTP version where every call is `http.Post → check status → parse response`. About half the lines, all of the visibility of "this is a Dapr call" preserved.
- **SDK defaults to gRPC on :50001, not HTTP on :3500.** `daprd client initializing for: 127.0.0.1:50001` in the boot log. Faster, typed, and this is now the transport for every Dapr call ingest-svc makes.
- **Seeding state at startup Just Worked.** `seedCostCenters` fires 7 `SaveState` calls before the HTTP server starts. State-redis handled it without complaint; sidecar didn't crash. Contrast with T5's pubsub-rabbitmq init crash — state stores are more forgiving than pub/sub.
- **CloudEvents envelope for `line-item.enriched` is completely automatic.** We pass a `[]byte` to `PublishEvent`, Dapr wraps it in `{id, source=ingest-svc, type=com.dapr.event.sent, datacontenttype, data, time}`. Zero effort on our side.
- **Shared Go module (`services/shared/finops`) lets us unit-test the domain layer with zero Dapr.** 7 test cases, sub-second, in-memory. Enrich is a pure function with a `LookupFunc` interface — the state-store implementation is a 10-line adapter and can be mocked to a Go map in tests.
- **Data generator in Python (~90 lines).** Deterministic with `--seed`, includes `--unmapped-pct` to inject items that should hit the "unmapped" counter (proves the counter works). This is our first polyglot moment — Python + Go, in the same repo, sharing only the JSON schema on the wire. That's exactly the Dapr pitch for polyglot systems: **the wire format is the contract**.

### Cons / Gotchas

- **Every line item costs 1 state.GET (lookup) + 1 state.SET (save) + 1 pubsub.PUBLISH.** Three sidecar round-trips per item. Fine for 20 items; something to think about for 100k/day. In a real FinOps ingest, we'd batch the state writes and possibly cache the lookup in-process (with TTL and Dapr Configuration change notifications). Batch state writes are a Dapr feature we haven't touched yet — `client.SaveBulkState` exists.
- **Go modules + local shared package requires an explicit `replace` directive**, even with `go.work`. Without it, `go mod tidy` tried to fetch the shared module from GitHub and blew up. Learned the hard way — added a note in the ingest-svc `go.mod` and the Dockerfile copies both modules into the image. If we add rollup-svc/triage-svc/notifier-svc, they'll all need the same `replace`.
- **Dapr SDK version mismatch risk.** Chose `github.com/dapr/go-sdk v1.11.0` matching our runtime `1.18.1` (SDK versions don't line up with runtime versions — this took a Google search). Every new SDK addition will need a compatibility check.
- **Something is injecting a duplicate `package main` line into new Go files on save.** Bit us twice today (sub-svc/main.go in T5, ingest-svc/main.go here) and once in shared/finops/lineitem.go. Not a Dapr issue at all — VS Code language server or a formatter is doing it. Watching for a pattern; if it keeps happening I'll try to root-cause the editor config.
- **Publishing to an unsubscribed topic silently drops messages.** RabbitMQ's Dapr integration uses a fanout exchange — with no queues bound, publish succeeds but the message goes nowhere. Our T7 verify shows `messages_ready=0` because no subscriber exists yet (rollup-svc arrives in T8). Not a bug; expected behaviour. But if we ever debug "why isn't my subscriber getting messages", check the queue exists before the publish.

### Overhead

- ingest-svc idles at ~15 MiB, sidecar ~40 MiB. Sending 20 line items in one batch: end-to-end round-trip ~200 ms (dominated by the 20× state.GET for lookups). Not tuned; not aiming to.

### Meta

- First slice with domain logic and TDD-flavored implementation. Wrote `Enrich` + tests first, then the Dapr adapter around it. When rollup-svc arrives in T8, the same pattern will apply — pure `Rollup(current, event) → next` in shared, thin Dapr adapter in the service.
- The demo has finally graduated from "smoke rigs" to "something that does a thing you'd point at in a real conversation." From here every slice adds business value on top of a working spine, not more smoke.

---

## T8 — `rollup-svc`: subscriber + aggregation + idempotency

_2026-07-04_

Full cross-service pub/sub flow now runs: `ingest-svc → daprd → rabbitmq → daprd → rollup-svc → state-postgres`. Rollups accumulate per `(day, team, service)`. Re-running the same batch does not double-count.

### Pros

- **Declarative Subscription CRD scoped by app-id.** One YAML file (~15 lines) routes a topic to a specific service and specific path. No client-side broker code, no subscription-manager service, no consumer-group ceremony. `scopes: [rollup-svc]` prevents accidental cross-delivery.
- **Pure `Rollup.Merge` in shared/finops → 8 unit tests, sub-second.** Same TDD-flavored pattern as T7: pure algebra in `shared`, thin Dapr wrapper in the service. The Dapr adapter is ~130 lines of Go and handles envelope parsing, idempotency, ETag concurrency, and status responses.
- **Dapr's Subscription response protocol (`SUCCESS`/`RETRY`/`DROP`) is the right shape.** Poison messages (bad envelope from the pre-fix ingest publish) got `DROP`'d and were ACKed instead of jamming the queue with permanent retries. Transient errors return `RETRY`. Real backpressure semantics without inventing them.
- **ETag optimistic concurrency for the rollup upsert works cleanly.** GET → merge → SaveStateWithETag → conflict? re-read + retry. About 12 lines of Go. Concurrent updates to the same rollup key are safe; readers see a consistent snapshot.
- **Idempotency in ~10 lines.** FirstWrite on `processed:<line-item-id>`; if the write conflicts, it's a duplicate delivery — increment counter, ACK, skip. Verify proved it: re-running `--seed 42` produced 20 duplicates and zero double-counting.

### Cons / Gotchas

- **`PublishEvent(pubsub, topic, []byte)` embeds the payload as a JSON-encoded string, not an object.** Cost me one debug cycle: the subscriber's `data json.RawMessage → EnrichedLineItem` unmarshal failed with `cannot unmarshal string into Go value of type EnrichedLineItem`. The Dapr Go SDK does NOT set `datacontenttype: application/json` when the arg is `[]byte` — it treats it as opaque. Fix: pass the struct directly (`dapr.PublishEvent(ctx, pubsub, topic, enriched)`) and let the SDK marshal + set content type. **Rule of thumb: never pass pre-marshalled `[]byte` to `PublishEvent` if the payload is JSON.**
- **The `meta map[string]string` argument to `SaveState` is NOT where concurrency options live.** I burned a debug cycle passing `meta: {"concurrency": "first-write"}` — Dapr silently ignored it (meta is component-specific request metadata, not state operation options). Concurrency belongs in the variadic `opts` via `daprd.WithConcurrency(daprd.StateConcurrencyFirstWrite)`. The two-slot API is subtle and easy to get wrong.
- **Conflict-error text varies by backend.** postgres v2 returns `"no item was updated"` (from `INSERT ON CONFLICT DO NOTHING` returning zero rows affected). Redis returns `"possible etag mismatch"`. There's no typed error to check — you're pattern-matching against strings. This is a real Dapr abstraction leak: **the wire is uniform, but error semantics leak the backend.** Wrote a defensive `isConcurrencyConflict` that ORs the known strings.
- **Dapr postgres v2 stores `value` as `bytea`, not `jsonb`.** Query time you need `convert_from(value,'UTF8')::jsonb->>'field'`. Compare with Redis where values are just strings you can `GET` directly. Grafana dashboards on Postgres (T10) will bake this cast into every panel.
- **On startup, if RabbitMQ is not yet routing to rollup-svc's queue, ingest-svc publishes go into the exchange and evaporate.** No error at the publisher; from ingest's `/stats` everything looks fine. Only visible from rollup-svc's `received` counter being lower than ingest's `published`. Real "silent" failure mode. Nothing new — same shape as T5's note about unsubscribed topics — but now we have counters on both sides that let us diagnose it.

### Overhead

- rollup-svc idle ~10 MiB, sidecar ~40 MiB. Adds one more (app + sidecar) pair to the cluster. Cluster total now around 3.7 GiB — Dapr sidecars are consistently ~40 MiB each so per-service overhead is predictable.

### Meta

- With T7 + T8, the flow is now: **20 line items in → 20 enriched line items persisted + 24 rollups per team/service/day → all traced end-to-end, idempotent under replay.** That's a real service. Grafana dashboards (T10) will visualise the rollups; anomaly detection (T9) will consume them.

### Bonus idempotency observation (post-verify)

After Tilt rebuilt rollup-svc with the isConcurrencyConflict fix, its in-memory counters reset to zero. Then a few seed operations came in and produced:

```
{"applied":0,"bad_env":0,"duplicate":60,"failed":0,"received":60}
```

Every one of those 60 events had a line-item ID whose `processed:<id>` marker was already sitting in Postgres from before the restart. So a **freshly-started rollup-svc pod** correctly refused to double-count, without any warm-up or state hydration. This matters because:

- Idempotency state lives in an external store, not the process — pod restarts don't reset it
- If we scaled to N rollup-svc replicas, they'd share the idempotency state through Postgres, no coordination needed
- Rolling deploys don't cause double-processing

Getting this property "for free" from `FirstWrite` on a Dapr state store is genuinely a nice piece of the pitch.

---

## T9 — Anomaly detection: event-driven + batch, idempotent

_2026-07-04_

Pure `Detect(current, history, cfg, now) → *Anomaly` in shared/finops; rollup-svc wires it into two triggers (per-event on rollup update, and `POST /detect?day=...` batch). Anomalies dedupe via `FirstWrite` on `anomaly:<day>:<team>:<service>` and publish to `anomaly.detected` on RabbitMQ. Verified end-to-end with a Python `--spike` flag that injects a 4× cost multiplier on `cc-payments-001/ec2`.

### Pros

- **Pure `Detect` in the shared package.** 7 test cases: happy path, at-threshold, below-threshold, below-floor, empty history, zero-cost, mixed-value history. Sub-second, no Dapr, no I/O. Same pattern as `Enrich` and `Rollup.Merge` — the Dapr adapter is a thin wrapper.
- **Two triggers, one detection function.** Event-driven detection fires as soon as a rollup crosses the threshold (near-real-time). Batch (`POST /detect?day=...`) scans the whole (team × service) grid for a day — useful for backtesting, deterministic verification, and cron-driven end-of-day passes. Both call the same `runDetection` code path.
- **Anomaly ID is deterministic** (`anomaly:<day>:<team>:<service>`) so FirstWrite gives us exactly-once semantics across triggers. Verified: cleared markers, batch detected 10 anomalies (`detected: 10`); ran again immediately, all 10 came back as `duplicate: 10`. Same pattern as line-item idempotency in T8 — this repo now has three layers of dedupe (processed-lines, rollup ETag, detected-anomalies), all using the same Dapr primitive.
- **Tunable via env vars** (`ANOMALY_PCT_THRESHOLD`, `ANOMALY_MIN_BASELINE_USD`, `ANOMALY_BASELINE_DAYS`) so a demo can walk through sensitivity settings without redeploying.
- **The `--spike CC:SVC:MULT` generator flag proved anomaly injection is deterministic.** `make anomaly-demo` runs backfill → 4× spike → batch detect and reliably surfaces the seeded anomaly as a top hit (877% over baseline in our run).

### Cons / Gotchas

- **Cross-app state access is impossible under Dapr's default `keyPrefix: appid`.** rollup-svc originally tried to read cost-center reference data from `state-redis` — the same store ingest-svc seeded. Every call returned nothing. Cause: Dapr auto-prefixes keys with the calling app-id. ingest-svc writes `ingest-svc||cost-center:cc-payments-001`; rollup-svc reads `rollup-svc||cost-center:cc-payments-001`. Different keyspaces, no overlap. **Options to share reference data across services:**
  1. Set `keyPrefix: none` on the Component — everyone shares one keyspace, isolation is gone
  2. Set `keyPrefix: <fixed-app-id>` — all services see one specific app-id's keyspace (better than none, still opinionated)
  3. Use a **separate** Component just for shared data (e.g., `state-shared`) with `keyPrefix: none`
  4. Mount the reference data via ConfigMap — bypass Dapr entirely for boot-time constants
  5. Service invocation: ask the owning service (`GET ingest-svc/lookups/...`) — always correct but a live-hop per read
  
  We chose #4 (ConfigMap) because the cost-center map is boot-time constant reference data, not a live database. Both ingest-svc and rollup-svc mount the same `cost-center-seed` ConfigMap; the JSON file is the source of truth. Nothing about Dapr changes; it's just not the right tool for this specific job. **This is a real Dapr abstraction limit worth calling out**: state stores are per-app by design, and the abstraction cannot pretend otherwise without giving up isolation.
- **Batch detect enumerates a hardcoded service list.** `knownServices = []string{"ec2","s3","rds","lambda","cloudfront"}` in rollup-svc must stay in sync with the generator's `SERVICES` list. Adding a new service means editing both. A more elegant solution would be discovering active `(team, service)` pairs from actual state keys — `QueryStateAlpha1` on postgres v2 supports this — but that's a T-something optimization, not T9. Noted.
- **RabbitMQ auto-provisions the `anomaly.detected` exchange on first publish, even with no subscribers.** Verified via `rabbitmqctl list_exchanges | grep anomaly` → the fanout exchange exists. Messages currently drop (no bound queues) — that changes when triage-svc arrives in T11. Not a bug; Dapr / RabbitMQ default behaviour. But if someone forgot to deploy the subscriber and expected retention, they'd be sad.
- **The event-driven trigger fires on EVERY rollup update**, not just once per (team, service, day). For a 100-line-item ingest with 4-5 items per (team, service), that's ~4-5 detection attempts per key, most producing zero new anomalies (only the last item might tip the total over threshold). Cheap in absolute terms — a GET on the day's rollup + 7 GETs for history = 8 state ops per event × ~100 events = 800 state calls per batch. Fine for the demo scale. In real FinOps we'd throttle to end-of-day + on-demand only.
- **Injected anomaly showed 877% over baseline, not the expected ~400% from a 4x multiplier.** Because the multiplier applies per-line-item, not per-rollup, and only to items whose `cost-center` tag AND `service` both match. Random sampling means the number of matching items varies day-to-day, so the aggregate delta isn't cleanly 4×. Not a bug — real FinOps anomalies have similar heterogeneity — but worth noting for anyone reading the demo output.
- **The `$10` `MinBaselineUSD` floor is a demo value, not a production one.** At real FinOps scale a static global floor is the wrong tool — sensible values differ by orders of magnitude across services (`analytics/rds` vs. `identity/lambda`). Production would want per-team or per-service floors, or something budget-driven ("alert at 80% of the daily budget"), or seasonal baselines (weekday vs. weekend). It's tunable at runtime via `ANOMALY_MIN_BASELINE_USD` on rollup-svc, so at least you can experiment without a redeploy — but the shape of the config, not just the value, is a demo simplification that would evolve.

### Overhead

- No new pods (detection lives inside rollup-svc). rollup-svc memory rose slightly (~5 MiB) from the ConfigMap read + detection logic. No new sidecar cost. Cluster total unchanged from T8.

### Meta

- 3 idempotency mechanisms now, all via `FirstWrite` on Dapr state:
  1. `processed:<line-item-id>` — no rollup double-count on message redelivery
  2. Rollup upsert with ETag — no lost updates under concurrent handlers
  3. `anomaly:<day>:<team>:<service>` — no duplicate anomaly publish across trigger types
  Each is ~5 lines of Go. **Dapr state's concurrency options are the single most valuable feature we've exercised for correctness so far.**
- Ready for T10 (Grafana dashboard on the rollups) and T11 (triage-svc subscribing to `anomaly.detected` and driving a workflow).

---

## T10 — Grafana FinOps dashboard on Postgres

_2026-07-04_

Provisioned dashboard, 7 panels, all pointing at the `state-postgres` DB via a Postgres datasource. Sidecar-based provisioning through a labelled ConfigMap so the dashboard survives cluster teardown and lives entirely in the repo.

### Pros

- **Zero manual clicking.** `deploy/infra/grafana-dashboards/finops-overview.json` is the whole dashboard; `make dashboards-install` wraps it into a `grafana_dashboard=1`-labelled ConfigMap in the monitoring namespace, and the kube-prom-stack Grafana sidecar picks it up within seconds. Same lifecycle as everything else in the repo.
- **Same Postgres Dapr writes to is directly queryable.** The T4 pro ("data is really in the backend, plaintext") pays off here: Grafana queries `SELECT convert_from(value,'UTF8')::jsonb->>'team_id' ...` against the exact rows rollup-svc writes. No dedicated read model, no ETL, no Dapr on the read path at all.
- **Template variable `$latest_day` makes "today" concrete in every panel title.** Query: `SELECT MAX(day) FROM rollups`. Every stat becomes "Total Spend — 2026-07-04" instead of the ambiguous "Total Spend Today". Also exposes a dropdown so demoers can pick any prior day and all "latest day" panels re-render.

### Cons / Gotchas

- **Timezone-in-`now()` is a real trap for demo dashboards.** First cut used `WHERE day = to_char(now(), 'YYYY-MM-DD')`, which evaluates in the Postgres pod's timezone (UTC). If you're running the demo in PDT after ~5 PM, `now()` in the pod is already tomorrow, the filter matches no rows, and "Total Spend Today" shows $0. Fix: don't rely on server-side clock — either use `MAX(day)` from the data itself (what we did), or explicitly cast: `now() AT TIME ZONE 'America/Los_Angeles'`. **Lesson: the data's notion of "current" is what you should trust, not the DB's.**
- **Grafana v11+ moved the Postgres `database` field.** Old provisioning YAML had `database: state` at the top level of the datasource definition; v11 requires `jsonData.database: state`. Symptom: the UI shows "*You do not currently have a default database configured for this data source*" and every query returns "no database" errors. Not documented in a way that surfaces on the datasource config page — you have to know to look in the migration notes. Same class of leak as the Dapr version-mismatch gotcha from T8: unclear where the docs say the truth.
- **Colour thresholds on stats need per-panel thought.** Default reflex is green→orange→red on numeric stats, which makes sense for **anomalies** (red means "action needed") but is misleading for **total spend** (spending money isn't inherently red-alarm-worthy). Made Total Spend / Line Items neutral-blue, kept red on Anomaly Count. Rule: **thresholds only where "high == bad" is universally true.**
- **Dashboard JSON is verbose and lightly documented.** Hand-authoring 300 lines of Grafana schema-v39 JSON is tedious and error-prone; typos silently produce empty panels rather than clear errors. Real teams typically manage dashboards via terraform-grafana or grafanactl. For a demo, hand-writing was fastest. Not scalable past 5-10 dashboards.

### Overhead

- No new pods. One ConfigMap (~10 KiB). Sidecar-driven reload is nearly instant. Zero Dapr surface.

### Meta

- With the dashboard live, the FinOps story is now demonstrable end-to-end without terminal commands: seed data via `make seed`, then just open the dashboard and watch totals move. That's what the pitch has been building toward.
- Ready for T11 — triage-svc will subscribe to the `anomaly.detected` topic that's currently un-consumed, and the workflow story finally arrives.

---

## T11 — triage-svc + Dapr Workflow (trivial per-anomaly instance)

_2026-07-05_

Third domain service. Subscribes to `anomaly.detected`, and for each event schedules a Dapr Workflow instance with a deterministic ID. The workflow itself is intentionally minimal (log input, return output) — T12 grows it into the real notify→wait→escalate loop.

### Pros

- **Workflow-as-code lands.** `TriageWorkflow(ctx *workflow.WorkflowContext) (any, error)` is a plain Go function. Dapr handles persistence, scheduling, actor placement, timers — all invisible from inside the workflow. Compared to hand-rolling this via `workflow_instances` tables + cron + callback handlers, the code-to-behaviour ratio is genuinely striking.
- **Deterministic instance IDs = fourth idempotency layer.** `triage-<anomaly-id>` (with colons replaced by dashes). Same anomaly delivered twice → `ScheduleNewWorkflow` returns `instance already exists` error → our `isDuplicateInstance` check ACKs and counts. Same shape as `FirstWrite` on state, `ETag` on rollup upsert, and processed-marker for line items. **Same primitive rediscovered in the workflow subsystem.**
- **Workflow state is queryable via one API call.** `FetchWorkflowMetadata(ctx, id)` returns the whole instance record (name, status, timestamps, serialized input/output, custom status, failure details) as a typed struct. No custom "workflow_status" table to maintain.
- **The T3 prediction cashed in exactly.** We flagged "Workflows require actor infrastructure even though we deferred actors" back in T3's NOTES. T11 hit it: `state store is not configured to use the actor runtime. Have you set the - name: actorStateStore value: "true"`. Diagnostic error message actually pointed at the fix, which is refreshingly better than most Dapr errors we've seen.
- **41 anomalies → 41 workflow instances → 0 failures in verify.** End-to-end throughput demonstration.

### Cons / Gotchas

- **`actorStateStore: "true"` is a required-but-easy-to-miss piece of Dapr config.** Any state store that will host actor state — including workflow state, since Dapr Workflows are built on actors — needs this metadata flag. Not on by default. Not surfaced anywhere unless you try to use workflows or actors. Once you know it, one line of YAML. Before you know it, an internal error that doesn't tell you which component to touch until you read carefully.
- **The workflow SDK API is different from the Dapr client SDK API.** Different import path (`github.com/dapr/go-sdk/workflow` vs `github.com/dapr/go-sdk/client`), different concepts (Worker vs Client, WithInstanceID/WithInput as free functions instead of options structs). Documentation is spread across two locations. Not hard once oriented; not obvious the first time.
- **`WorkflowContext.SetCustomStatus` doesn't exist in Go SDK v1.11.0** despite being in the docs. First hit at compile time. Removed and used the return value as the "status" instead.
- **Instance IDs must be `[a-zA-Z0-9_-]+` — colons and other separators are rejected.** Our anomaly IDs are `anomaly:<day>:<team>:<service>`. Had to hand-translate `:` → `-` for the instance ID.
- **No workflow list API.** You can `Schedule`, `Fetch(id)`, `RaiseEvent`, `Terminate`, `Purge` — no `List`. Peeking at Redis keys via `redis-cli KEYS '*triage*'` works but is implementation-detail. T11.5 will add a self-managed workflow inbox as the mitigation.
- **Workflow deployment DOES restart cleanly**, but the state that identifies an actor host lives in placement. So on rollout, in-flight workflows briefly pause while placement re-elects a host. Under demo-scale traffic, invisible; under real load, worth measuring.

### Overhead

- triage-svc adds one more (app + sidecar) pair. Sidecar sits at ~40 MiB like the others; app is ~15 MiB idle. Cluster total ~3.9 GiB now.
- Every workflow instance is a virtual actor. State lives in Redis (state-redis, now flagged as `actorStateStore`). Each of the 41 completed instances is ~1-2 KB of Redis. Not measured precisely; not concerning.

### Meta

- **This is the T3 prediction cashing in.** Reading T3's NOTES: "Workflows require actor infrastructure even though we deferred actors." That observation was speculative when we made it. T11 confirmed it exactly, and gave us the concrete error message to point at in a pitch. **The whole NOTES.md discipline is now paying off — earlier speculative gotchas are becoming testable predictions.**
- Ready for T11.5 (workflow inbox, ~30 min follow-up commit), then T12 (real workflow: notify → wait for ack → escalate with HTMX ack button).
