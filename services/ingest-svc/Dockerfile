# ingest-svc Dockerfile — same shape as hello/sub, adds a shared-package COPY.
FROM dapr-stuff/base-builder AS builder
WORKDIR /workspace

# Pull the shared module in first so it lives at the path the ingest-svc
# go.mod's local `replace` (../shared) expects.
COPY shared/ services/shared/
COPY ingest-svc/ services/ingest-svc/

WORKDIR /workspace/services/ingest-svc
RUN go mod download && \
    go build -ldflags="-s -w" -o /out/ingest .

FROM dapr-stuff/base-runtime
COPY --from=builder /out/ingest /usr/local/bin/ingest
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ingest"]
