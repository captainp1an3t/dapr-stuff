# notifier-svc Dockerfile — Python + Dapr Python SDK.
# Doesn't inherit from the Go bases (base.Dockerfile) because Python and Go
# have completely different runtime needs; we install the extra CAs inline
# using the same pattern our Go bases use.

FROM python:3.12-alpine

# Extra trusted CAs (from .ca-extras.pem at repo root — see ADR-0001).
# Same append-to-existing-bundle trick as base.Dockerfile: never `apk add
# ca-certificates`, that would loop through the very proxy we're trying to
# trust.
COPY .ca-extras.pem /usr/local/share/ca-certificates/extras.crt
RUN cat /usr/local/share/ca-certificates/extras.crt >> /etc/ssl/certs/ca-certificates.crt

# Python-specific CA envs — libraries don't all honour the same one:
#   REQUESTS_CA_BUNDLE — used by `requests`
#   SSL_CERT_FILE      — used by stdlib ssl / urllib3
#   PIP_CERT           — used by pip during install
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt \
    PIP_CERT=/etc/ssl/certs/ca-certificates.crt \
    PYTHONUNBUFFERED=1

WORKDIR /app

COPY services/notifier-svc/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY services/notifier-svc/notifier.py .
COPY services/notifier-svc/test_notifier.py .

EXPOSE 8080
ENTRYPOINT ["python", "notifier.py"]
