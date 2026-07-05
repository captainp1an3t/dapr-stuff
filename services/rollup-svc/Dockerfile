# rollup-svc Dockerfile — same shape as ingest-svc.
FROM dapr-stuff/base-builder AS builder
WORKDIR /workspace

COPY shared/ services/shared/
COPY rollup-svc/ services/rollup-svc/

WORKDIR /workspace/services/rollup-svc
RUN go mod download && \
    go build -ldflags="-s -w" -o /out/rollup .

FROM dapr-stuff/base-runtime
COPY --from=builder /out/rollup /usr/local/bin/rollup
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/rollup"]
