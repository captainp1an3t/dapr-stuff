# Additional trusted CA certificates baked into images and mounted into Dapr sidecars

Development can happen behind a TLS-intercepting proxy, so any egress from a container — package installs during builds, image pulls inside the KinD node, Dapr sidecar HTTP calls, app runtime TLS — fails without the intercepting CA in the container's trust store. Whatever extra CAs the local network requires are placed at `.ca-extras.pem` at the repo root (a single concatenated PEM bundle). `make prep` ensures the file exists — empty by default — and every consumer treats an empty bundle as a no-op:

- **Application images** — the shared base stage does `COPY .ca-extras.pem /usr/local/share/ca-certificates/extras.crt` and `cat >> /etc/ssl/certs/ca-certificates.crt`. Appending directly to Alpine's existing bundle side-steps the chicken-and-egg of `apk add ca-certificates` needing the proxy CA to already be trusted. If the source file is empty, the append is a no-op.
- **KinD node containerd** — immediately after cluster creation `cluster-trust-cas` does `docker cp` + `update-ca-certificates` + `systemctl restart containerd` inside each node, so containerd trusts any registry the proxy MITMs. Skipped when `.ca-extras.pem` is empty.
- **Dapr sidecars that need egress** — mounted as a Kubernetes `Secret` and pointed at via `SSL_CERT_FILE` / `SSL_CERT_DIR`. Same skip semantics.

This is deliberately verbose because the failure mode without it — opaque `x509: certificate signed by unknown authority` errors scattered across build logs, containerd, sidecars, and app runtimes — is much worse than the one-time cost of doing it consistently everywhere.

## Considered alternatives

- **Per-Dockerfile duplication (no shared base).** Rejected: 3 identical lines repeated across every service is exactly the tribal-knowledge trap this ADR exists to avoid.
- **Runtime-only mounting (no image baking).** Rejected: doesn't help with build-time `apk add` / `go mod download` failures, which is where most developers hit this first.
- **`kind load docker-image` for third-party images instead of trusting the CA in the node.** Considered and initially chosen for v1, then reversed within the first observability install: kube-prometheus-stack alone pulls from `quay.io/kiwigrid`, `quay.io/prometheus-operator`, `quay.io/prometheus`, `registry.k8s.io/kube-state-metrics`, and `docker.io/grafana`. Curating a preload list for every future Helm chart is a treadmill. Trusting the CA in the node once, at cluster creation, is a single-cost fix that covers every current and future registry.
- **`containerdConfigPatches` in the KinD config, per-registry.** Rejected as unnecessary complexity once the node-wide trust-store approach was chosen — configuring the trust store makes containerd Just Work without per-registry config.
- **Committing an in-tree CA bundle.** Rejected: attributes local infrastructure knowledge to whoever pushes the repo publicly. The gitignored `.ca-extras.pem` (populated by whatever local mechanism the operator prefers) is the mitigation.

## Consequences

- Every app image inherits from an internal base image stage that installs the extras. New services must follow the same pattern.
- The bundle is provided out-of-band at `.ca-extras.pem`; the repo never carries it. `make prep` ensures the file exists so build steps don't fail on missing file.
- Language-specific runtime envvars must be set per service: Go (`SSL_CERT_FILE`), Node (`NODE_EXTRA_CA_CERTS`), Python (`REQUESTS_CA_BUNDLE`), .NET (its own dance). Document per-service as they're added.
- The `cluster-create` and Docker-build steps depend on `make prep` having run. `make up` chains this automatically; ad-hoc `docker build` needs `make prep` first.
- On a clone with no `.ca-extras.pem` populated, everything still builds and runs; the extra-CA install steps become no-ops.
