"""Unit tests for notifier.build_slack_payload — pure-function coverage."""

import pytest

from notifier import build_slack_payload


BASE_ANOMALY = {
    "day": "2026-07-04",
    "team_id": "team-payments",
    "team_name": "Payments Platform",
    "service": "ec2",
    "actual_cost_usd": 7565.42,
    "baseline_cost_usd": 850.00,
    "delta_pct": 789.5,
}


def test_initial_kind_has_cost_anomaly_prefix_and_warning_color():
    p = build_slack_payload(BASE_ANOMALY, "initial")
    assert "[COST ANOMALY]" in p["text"]
    assert p["attachments"][0]["color"] == "warning"


def test_escalation_kind_has_escalation_prefix_and_danger_color():
    p = build_slack_payload(BASE_ANOMALY, "escalation")
    assert "[ESCALATION]" in p["text"]
    assert p["attachments"][0]["color"] == "danger"


def test_text_includes_team_service_day_and_pct():
    p = build_slack_payload(BASE_ANOMALY, "initial")
    text = p["text"]
    assert "Payments Platform" in text
    assert "ec2" in text
    assert "2026-07-04" in text
    assert "+790%" in text  # rounded from 789.5
    assert "$7,565.42" in text
    assert "$850.00" in text


def test_falls_back_to_team_id_when_team_name_missing():
    a = {**BASE_ANOMALY, "team_name": None}
    p = build_slack_payload(a, "initial")
    assert "team-payments" in p["text"]


def test_missing_fields_do_not_crash():
    # Absolute minimum — only kind, no anomaly data
    p = build_slack_payload({}, "initial")
    assert "text" in p
    assert "unknown" in p["text"]  # fallback for team


def test_fields_are_all_present_with_expected_titles():
    p = build_slack_payload(BASE_ANOMALY, "initial")
    titles = [f["title"] for f in p["attachments"][0]["fields"]]
    assert titles == ["Team", "Service", "Day", "Delta", "Actual", "Baseline"]


@pytest.mark.parametrize(
    "delta_pct,expected",
    [(50.4, "+50%"), (100.6, "+101%"), (-25.3, "-25%"), (0, "+0%")],
)
def test_delta_pct_rounding(delta_pct, expected):
    a = {**BASE_ANOMALY, "delta_pct": delta_pct}
    p = build_slack_payload(a, "initial")
    assert expected in p["text"]


# ---- T14 optimisation payload tests ----------------------------------------
from notifier import build_optimisation_payload

BASE_OPTIMISATION = {
    "team_id": "team-payments",
    "team_name": "Payments Platform",
    "service": "ebs",
    "resource_id": "vol-abc123",
    "resource_type": "EBS volume",
    "monthly_waste_usd": 42.50,
    "days_idle": 45,
    "suggested_action": "delete",
}


def test_optimisation_text_includes_key_facts():
    p = build_optimisation_payload(BASE_OPTIMISATION, "optimisation-request")
    text = p["text"]
    assert "Payments Platform" in text
    assert "EBS volume" in text
    assert "vol-abc123" in text
    assert "ebs" in text
    assert "45 days" in text
    assert "$42.50" in text
    assert "delete" in text
    assert "[OPTIMISATION]" in text


def test_optimisation_field_titles():
    p = build_optimisation_payload(BASE_OPTIMISATION, "optimisation-request")
    titles = [f["title"] for f in p["attachments"][0]["fields"]]
    assert "Team" in titles
    assert "Resource" in titles
    assert "Monthly waste" in titles
    assert "Suggested action" in titles
    # None of the anomaly-specific fields leak in.
    assert "Delta" not in titles
    assert "Baseline" not in titles


def test_optimisation_missing_fields_do_not_crash():
    p = build_optimisation_payload({}, "optimisation-request")
    assert "text" in p
    assert p["attachments"][0]["color"] == "good"


def test_optimisation_uses_team_id_when_team_name_missing():
    opt = {**BASE_OPTIMISATION}
    opt.pop("team_name")
    p = build_optimisation_payload(opt, "optimisation-request")
    assert "team-payments" in p["text"]
