# triage-svc Dockerfile — same shape as ingest-svc / rollup-svc.
FROM dapr-stuff/base-builder AS builder
WORKDIR /workspace

COPY shared/ services/shared/
COPY triage-svc/ services/triage-svc/

WORKDIR /workspace/services/triage-svc
RUN go mod download && \
    go build -ldflags="-s -w" -o /out/triage .

FROM dapr-stuff/base-runtime
COPY --from=builder /out/triage /usr/local/bin/triage
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/triage"]
