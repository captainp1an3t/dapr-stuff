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
