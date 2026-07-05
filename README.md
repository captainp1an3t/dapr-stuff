# dapr-stuff

Learning vehicle for [Dapr](https://dapr.io), built as a deliberately shallow FinOps sample application. See [CONTEXT.md](CONTEXT.md) for the "why" and [tasks/dapr-finops-v1.md](tasks/dapr-finops-v1.md) for the tracer-bullet task breakdown.

## Prerequisites

- Docker (Docker Desktop or equivalent) running
- [KinD](https://kind.sigs.k8s.io/) — `brew install kind`
- [Tilt](https://tilt.dev/) — `brew install tilt-dev/tap/tilt`
- `kubectl`, `helm`
- Go (>=1.26)

## Quick start

```bash
make up       # Creates KinD cluster and launches Tilt (blocks until Ctrl-C)
make verify   # In another shell — smoke-tests the current stack
make down     # Tears everything down
```

`make help` lists all targets.

## Dev environments behind a TLS-intercepting proxy

If your local network sits behind a corporate MITM proxy (or you otherwise need to trust additional CAs at build/runtime), place a concatenated PEM bundle at:

```
.ca-extras.pem
```

before running `make up`. The bundle is baked into every service image and installed into the KinD node's system trust store; both application code and containerd will trust it. If the file is absent or empty, `make prep` creates it empty and every consumer treats it as a no-op — the stack builds against public CAs only.

See [docs/adr/0001-ca-extras.md](docs/adr/0001-ca-extras.md) for the full rationale.

Neither `.ca-extras.pem` nor any local scripts you use to produce it are committed to the repo (gitignored).
