#!/usr/bin/env python3
"""Synthetic FinOps line-item generator.

Emits NDJSON line items matching the schema in services/shared/finops/lineitem.go
and POSTs them to ingest-svc's /ingest endpoint (or dumps to stdout if --output
is provided).

Usage:
    python generate.py --day 2026-07-04 --count 100
    python generate.py --day 2026-07-04 --count 100 --output items.ndjson
    python generate.py --day 2026-07-04 --count 100 --url http://localhost:8080/ingest
    python generate.py --day 2026-07-04 --count 100 --seed 42 --unmapped-pct 5
"""

from __future__ import annotations

import argparse
import json
import random
import sys
from pathlib import Path
from typing import Iterable
from urllib import request

# Should match the ConfigMap in deploy/apps/ingest-svc.yaml. Kept small on
# purpose — 7 cost centers is enough for a demo.
KNOWN_COST_CENTERS = [
    "cc-payments-001",
    "cc-search-001",
    "cc-checkout-001",
    "cc-catalog-001",
    "cc-identity-001",
    "cc-analytics-001",
    "cc-platform-001",
]

# A cost center that intentionally is NOT seeded — used when --unmapped-pct > 0
# so we can prove ingest-svc's "unmapped" counter works end-to-end.
UNKNOWN_COST_CENTER = "cc-legacy-999"

SERVICES = ["ec2", "s3", "rds", "lambda", "cloudfront"]
ENVIRONMENTS = ["prod", "staging", "dev"]
UNITS = ["Hrs", "GB-Mo", "Requests"]


def generate(
    day: str, count: int, rng: random.Random, unmapped_pct: int
) -> Iterable[dict]:
    for seq in range(count):
        # Some fraction of items get an unknown cost-center; a smaller fraction
        # get no cost-center tag at all. Both count as "unmapped" downstream.
        r = rng.random() * 100
        if r < unmapped_pct * 0.7:
            cc = UNKNOWN_COST_CENTER
        elif r < unmapped_pct:
            cc = None
        else:
            cc = rng.choice(KNOWN_COST_CENTERS)

        tags: dict[str, str] = {"environment": rng.choice(ENVIRONMENTS)}
        if cc is not None:
            tags["cost-center"] = cc

        yield {
            "line_item_id": f"li-{day}-{seq:06d}",
            "day": day,
            "service": rng.choice(SERVICES),
            "cost_usd": round(rng.uniform(0.01, 500.0), 4),
            "quantity": round(rng.uniform(0.1, 100.0), 2),
            "unit": rng.choice(UNITS),
            "tags": tags,
        }


def to_ndjson(items: Iterable[dict]) -> bytes:
    return b"\n".join(json.dumps(item, separators=(",", ":")).encode() for item in items)


def post(url: str, body: bytes) -> None:
    req = request.Request(
        url, data=body, method="POST", headers={"Content-Type": "application/x-ndjson"}
    )
    with request.urlopen(req, timeout=30) as resp:
        print(f"HTTP {resp.status}", file=sys.stderr)
        print(resp.read().decode(), file=sys.stderr)


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--day", required=True, help="YYYY-MM-DD")
    p.add_argument("--count", type=int, default=100, help="number of items")
    p.add_argument("--seed", type=int, default=None, help="deterministic seed")
    p.add_argument(
        "--unmapped-pct",
        type=int,
        default=5,
        help="percent of items with unknown or missing cost-center (0-100)",
    )
    group = p.add_mutually_exclusive_group()
    group.add_argument(
        "--url",
        default="http://localhost:8080/ingest",
        help="POST target (default: %(default)s)",
    )
    group.add_argument(
        "--output",
        type=Path,
        help="write NDJSON to this file (or '-' for stdout) instead of POSTing",
    )
    args = p.parse_args()

    rng = random.Random(args.seed)
    items = list(generate(args.day, args.count, rng, args.unmapped_pct))
    body = to_ndjson(items)

    if args.output is not None:
        if str(args.output) == "-":
            sys.stdout.buffer.write(body)
        else:
            args.output.write_bytes(body)
            print(f"wrote {len(items)} items to {args.output}", file=sys.stderr)
        return 0

    print(f"POSTing {len(items)} items to {args.url}", file=sys.stderr)
    post(args.url, body)
    return 0


if __name__ == "__main__":
    sys.exit(main())
