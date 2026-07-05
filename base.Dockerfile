# base.Dockerfile — shared base images for all services in this repo.
#
# Provides two stages:
#   builder-base: golang:alpine + git + optional extra CAs appended to trust store.
#   runtime-base: alpine + optional extra CAs appended to trust store.
#
# The cert bundle comes from `.ca-extras.pem` at the repo root. If your dev
# environment sits behind a TLS-intercepting proxy, populate that file before
# running `make up` (see README). Otherwise `make prep` creates it empty and
# the `cat >>` below is a harmless no-op.
#
# NOTE: we deliberately DO NOT run `apk add ca-certificates` or
# `update-ca-certificates`. Alpine ships /etc/ssl/certs/ca-certificates.crt as
# part of its baselayout; installing the ca-certificates package would require
# fetching from the alpine repo, which fails when that fetch itself goes
# through a proxy whose CA isn't yet trusted. Appending directly to the
# existing bundle side-steps the chicken-and-egg.
#
# See docs/adr/0001-ca-extras.md for the full rationale.

ARG GOLANG_VERSION=1.26.2
ARG ALPINE_VERSION=3.20

# ---------- builder-base ----------
FROM golang:${GOLANG_VERSION}-alpine AS builder-base
COPY .ca-extras.pem /usr/local/share/ca-certificates/extras.crt
RUN cat /usr/local/share/ca-certificates/extras.crt >> /etc/ssl/certs/ca-certificates.crt && \
    apk add --no-cache git
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    GO111MODULE=on \
    CGO_ENABLED=0

# ---------- runtime-base ----------
FROM alpine:${ALPINE_VERSION} AS runtime-base
COPY .ca-extras.pem /usr/local/share/ca-certificates/extras.crt
RUN cat /usr/local/share/ca-certificates/extras.crt >> /etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
