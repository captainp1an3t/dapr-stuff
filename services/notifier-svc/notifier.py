"""notifier-svc — Python + Dapr

Called by triage-svc (via Dapr service invocation) when a workflow needs to
notify the owning team about a cost anomaly. Reads its "Slack webhook URL"
from the Dapr secret store on startup.

In this demo the secret is a placeholder pointing at example.local, so we
never actually hit Slack — we log the rendered payload into an in-memory
inbox (readable via GET /inbox) so the flow is observable end-to-end
without needing a real Slack workspace.

If SLACK_WEBHOOK_OVERRIDE is set (env var), we POST there instead of the
Dapr-stored value. Useful for pointing at mailhog / smee.io during
integration testing.
"""

from __future__ import annotations

import logging
import os
import threading
import time
from typing import Any

import requests
from dapr.clients import DaprClient
from flask import Flask, jsonify, request

# ---- Config -----------------------------------------------------------------

SECRET_STORE = os.environ.get("DAPR_SECRET_STORE", "secretstore-kubernetes")
SECRET_NAME = os.environ.get("DAPR_SECRET_NAME", "demo-secret")
SECRET_KEY = os.environ.get("DAPR_SECRET_KEY", "slack-webhook-url")
WEBHOOK_OVERRIDE = os.environ.get("SLACK_WEBHOOK_OVERRIDE", "").strip()
PORT = int(os.environ.get("PORT", "8080"))
INBOX_MAX = 100

# ---- App state --------------------------------------------------------------

app = Flask(__name__)
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

_inbox: list[dict[str, Any]] = []
_inbox_lock = threading.Lock()
_stats = {
    "received": 0,
    "delivered_real": 0,
    "delivered_mock": 0,
    "failed": 0,
    "secret_read": False,
    "webhook_source": "none",  # "override" | "dapr-secret" | "none"
}
_webhook_url: str | None = None


# ---- Startup ----------------------------------------------------------------

def load_webhook() -> None:
    """Resolve the Slack webhook URL once at startup.

    Precedence: SLACK_WEBHOOK_OVERRIDE env var > Dapr secret store > None.
    None means "log-only mode" — /notify still works, just records to inbox.
    """
    global _webhook_url

    if WEBHOOK_OVERRIDE:
        _webhook_url = WEBHOOK_OVERRIDE
        _stats["webhook_source"] = "override"
        app.logger.info("using webhook from SLACK_WEBHOOK_OVERRIDE env var")
        return

    try:
        with DaprClient() as d:
            resp = d.get_secret(store_name=SECRET_STORE, key=SECRET_NAME)
            _webhook_url = resp.secret.get(SECRET_KEY)
            _stats["secret_read"] = True
            _stats["webhook_source"] = "dapr-secret"
            app.logger.info(
                "loaded webhook via Dapr secret store=%s name=%s key=%s masked=%s",
                SECRET_STORE, SECRET_NAME, SECRET_KEY, mask_url(_webhook_url),
            )
    except Exception as e:  # noqa: BLE001 — startup, want the traceback in logs
        app.logger.exception("failed to load webhook secret: %s", e)
        raise


def mask_url(url: str | None) -> str:
    if not url:
        return "<empty>"
    if len(url) < 20:
        return "<short>"
    return f"{url[:15]}...{url[-4:]}"


# ---- HTTP handlers ----------------------------------------------------------

@app.route("/health")
def health():
    return jsonify(status="okay", service="notifier-svc")


@app.route("/stats")
def stats():
    return jsonify(_stats)


@app.route("/inbox")
def inbox():
    with _inbox_lock:
        recent = list(reversed(_inbox[-20:]))
    return jsonify(count=len(_inbox), items=recent)


@app.route("/notify", methods=["POST"])
def notify():
    _stats["received"] += 1
    body = request.get_json(force=True, silent=True) or {}
    kind = body.get("kind", "initial")

    # Polymorphic payload: {kind, anomaly} for T12 triage flow,
    # {kind, optimisation} for T14 optimisation flow. One /notify endpoint
    # so the abstraction at the workflow-activity level stays uniform;
    # discriminate here on payload shape rather than on kind. If kind and
    # payload shape ever conflict, payload shape wins.
    optimisation = body.get("optimisation") or {}
    anomaly = body.get("anomaly") or {}

    if optimisation:
        payload = build_optimisation_payload(optimisation, kind)
        subject_id = (
            f"{optimisation.get('team_id', '?')}:"
            f"{optimisation.get('resource_id', '?')}:"
            f"{optimisation.get('suggested_action', '?')}"
        )
    else:
        payload = build_slack_payload(anomaly, kind)
        subject_id = (
            f"{anomaly.get('day', '?')}:"
            f"{anomaly.get('team_id', '?')}:"
            f"{anomaly.get('service', '?')}"
        )

    entry = {
        "sent_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "kind": kind,
        "anomaly_id": subject_id,  # kept as field name for backwards compat
        "payload_text": payload.get("text"),
    }

    # Only actually POST if we have a real webhook (not the demo placeholder).
    is_real = bool(_webhook_url) and "example.local" not in (_webhook_url or "")
    if is_real:
        try:
            r = requests.post(_webhook_url, json=payload, timeout=10)
            r.raise_for_status()
            _stats["delivered_real"] += 1
            entry["delivered"] = "real"
        except Exception as e:  # noqa: BLE001
            app.logger.exception("real webhook POST failed: %s", e)
            _stats["failed"] += 1
            entry["delivered"] = "failed"
            entry["error"] = str(e)
            _record(entry)
            return jsonify(status="failed", error=str(e)), 502
    else:
        _stats["delivered_mock"] += 1
        entry["delivered"] = "mock"

    _record(entry)
    return jsonify(status="okay", kind=kind, delivered=entry["delivered"])


def _record(entry: dict[str, Any]) -> None:
    with _inbox_lock:
        _inbox.append(entry)
        if len(_inbox) > INBOX_MAX:
            del _inbox[: len(_inbox) - INBOX_MAX]


# ---- Payload builder — pure, unit-tested ------------------------------------

def build_slack_payload(anomaly: dict[str, Any], kind: str) -> dict[str, Any]:
    """Render a Slack-compatible message from an anomaly dict.

    `kind` is "initial" for first alert, "escalation" after timeout.
    """
    team = anomaly.get("team_name") or anomaly.get("team_id") or "unknown"
    service = anomaly.get("service", "?")
    day = anomaly.get("day", "?")
    delta_pct = float(anomaly.get("delta_pct", 0))
    actual = float(anomaly.get("actual_cost_usd", 0))
    baseline = float(anomaly.get("baseline_cost_usd", 0))

    prefix = "[ESCALATION] " if kind == "escalation" else "[COST ANOMALY] "
    text = (
        f"{prefix}*{team}* on `{service}` — day {day}, "
        f"${actual:,.2f} vs baseline ${baseline:,.2f} ({delta_pct:+.0f}%)"
    )
    color = "danger" if kind == "escalation" else "warning"

    return {
        "text": text,
        "attachments": [
            {
                "color": color,
                "fields": [
                    {"title": "Team", "value": team, "short": True},
                    {"title": "Service", "value": service, "short": True},
                    {"title": "Day", "value": day, "short": True},
                    {"title": "Delta", "value": f"{delta_pct:+.0f}%", "short": True},
                    {"title": "Actual", "value": f"${actual:,.2f}", "short": True},
                    {"title": "Baseline", "value": f"${baseline:,.2f}", "short": True},
                ],
            }
        ],
    }


def build_optimisation_payload(optimisation: dict[str, Any], kind: str) -> dict[str, Any]:
    """Render a Slack-compatible message from an IdleResource dict.

    `kind` for T14 is currently just "optimisation-request" — approval and
    rejection outcomes are recorded to state, not re-notified. If we later
    want confirmation pings, add "optimisation-approved" / "-rejected".
    """
    team = optimisation.get("team_name") or optimisation.get("team_id") or "unknown"
    service = optimisation.get("service", "?")
    resource_id = optimisation.get("resource_id", "?")
    resource_type = optimisation.get("resource_type", "resource")
    action = optimisation.get("suggested_action", "review")
    waste = float(optimisation.get("monthly_waste_usd", 0))
    days = int(optimisation.get("days_idle", 0))

    text = (
        f"[OPTIMISATION] *{team}* — {resource_type} `{resource_id}` on `{service}` "
        f"has been idle {days} days (~${waste:,.2f}/month waste). "
        f"Suggested action: *{action}*."
    )

    return {
        "text": text,
        "attachments": [
            {
                "color": "good",  # blue-ish informational, not an alarm
                "fields": [
                    {"title": "Team", "value": team, "short": True},
                    {"title": "Service", "value": service, "short": True},
                    {"title": "Resource", "value": f"{resource_type} {resource_id}", "short": False},
                    {"title": "Idle for", "value": f"{days} days", "short": True},
                    {"title": "Monthly waste", "value": f"${waste:,.2f}", "short": True},
                    {"title": "Suggested action", "value": action, "short": True},
                    {"title": "Kind", "value": kind, "short": True},
                ],
            }
        ],
    }


# ---- Main -------------------------------------------------------------------

if __name__ == "__main__":
    load_webhook()
    app.logger.info("notifier-svc listening on :%d", PORT)
    app.run(host="0.0.0.0", port=PORT)
