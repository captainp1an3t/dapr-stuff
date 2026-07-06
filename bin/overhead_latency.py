#!/usr/bin/env python3
"""T16 helper: latency probe with p50/p95/p99/max.

Hits a URL N times sequentially (or with light concurrency), records per-request
wall time, and prints percentiles. Uses `requests` (already a dep of notifier-svc).

Skips `ab` because ab-on-macOS-against-localhost-port-forwards has a consistent
~1s per-request artifact that swamps real latency signal (verified: both direct
and Dapr endpoints report identical ~1006ms via ab; curl reports ~15ms).
"""

from __future__ import annotations

import argparse
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor

import requests


def probe(url: str, n: int, concurrency: int, timeout: float) -> list[float]:
    """Return a list of per-request latencies in milliseconds."""
    latencies: list[float] = []
    session = requests.Session()

    def one() -> float:
        t0 = time.perf_counter()
        try:
            r = session.get(url, timeout=timeout)
            r.raise_for_status()
        except Exception as e:  # noqa: BLE001
            return -1.0
        return (time.perf_counter() - t0) * 1000

    if concurrency <= 1:
        for _ in range(n):
            latencies.append(one())
    else:
        with ThreadPoolExecutor(max_workers=concurrency) as pool:
            for lat in pool.map(lambda _: one(), range(n)):
                latencies.append(lat)

    return [x for x in latencies if x >= 0]


def summarise(latencies: list[float]) -> dict[str, float]:
    """Return a summary dict. Empty on no successful requests."""
    if not latencies:
        return {}
    return {
        "n": len(latencies),
        "mean": statistics.mean(latencies),
        "p50": statistics.median(latencies),
        "p95": _pct(latencies, 0.95),
        "p99": _pct(latencies, 0.99),
        "max": max(latencies),
        "min": min(latencies),
    }


def _pct(values: list[float], q: float) -> float:
    ordered = sorted(values)
    idx = min(len(ordered) - 1, int(len(ordered) * q))
    return ordered[idx]


def main() -> int:
    ap = argparse.ArgumentParser(description="latency probe (p50/p95/p99)")
    ap.add_argument("url", help="URL to probe")
    ap.add_argument("--n", type=int, default=200, help="request count (default 200)")
    ap.add_argument("--concurrency", type=int, default=1, help="parallel workers (default 1)")
    ap.add_argument("--timeout", type=float, default=5.0, help="per-request timeout seconds")
    ap.add_argument("--warmup", type=int, default=5, help="warmup requests (excluded from stats)")
    ap.add_argument("--label", default="", help="label to prefix the result line")
    args = ap.parse_args()

    # Warmup.
    if args.warmup > 0:
        probe(args.url, args.warmup, 1, args.timeout)

    lats = probe(args.url, args.n, args.concurrency, args.timeout)
    s = summarise(lats)
    if not s:
        print(f"    {args.label}: ALL REQUESTS FAILED against {args.url}")
        return 1

    label = args.label or args.url
    print(
        f"    {label:<50}"
        f" n={int(s['n']):>3d}"
        f"  p50={s['p50']:>5.1f}ms"
        f"  p95={s['p95']:>5.1f}ms"
        f"  p99={s['p99']:>5.1f}ms"
        f"  max={s['max']:>6.1f}ms"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
