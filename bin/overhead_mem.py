#!/usr/bin/env python3
"""T16 helper: query Prometheus for container memory and print a summary table.

Expects Prometheus reachable at http://localhost:19090 (via port-forward set up
by the `make overhead-memory` target). Prints a table of (pod, container, MiB)
plus totals and sidecar-overhead percentage.

Isolated as a script rather than a `python3 -c` blob because Makefile shell
mangling escapes double-quotes inside f-strings — see NOTES.md T12 gotcha.
"""

from __future__ import annotations

import json
import sys
import urllib.parse
import urllib.request

PROM_URL = "http://localhost:19090"
QUERY = (
    'container_memory_working_set_bytes'
    '{namespace="default",container=~"ingest|rollup|triage|notifier|daprd"}'
)


def main() -> int:
    url = PROM_URL + "/api/v1/query?query=" + urllib.parse.quote(QUERY)
    try:
        resp = json.loads(urllib.request.urlopen(url, timeout=5).read())
    except Exception as e:  # noqa: BLE001
        print(f"    ERROR querying Prometheus: {e}")
        return 1

    results = resp.get("data", {}).get("result", [])
    if not results:
        print("    (no memory metrics returned — check port-forward or namespace)")
        return 1

    rows: list[tuple[str, str, float]] = []
    for m in results:
        pod = m["metric"].get("pod", "?")
        container = m["metric"].get("container", "?")
        mib = float(m["value"][1]) / 1024 / 1024
        rows.append((pod, container, mib))
    rows.sort()

    print(f"    {'POD':<32} {'CONTAINER':<12} {'MiB':>8}")
    for pod, container, mib in rows:
        print(f"    {pod:<32} {container:<12} {mib:>8.1f}")

    apps = sum(mib for _, c, mib in rows if c != "daprd")
    sidecars = sum(mib for _, c, mib in rows if c == "daprd")
    total = apps + sidecars

    print(f"    {'TOTAL APPS':<45} {apps:>8.1f}")
    print(f"    {'TOTAL DAPRD SIDECARS':<45} {sidecars:>8.1f}")
    if total > 0:
        print(f"    {'SIDECAR OVERHEAD':<45} {sidecars / total * 100:>7.1f}%")
    return 0


if __name__ == "__main__":
    sys.exit(main())
