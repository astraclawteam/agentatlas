#!/usr/bin/env python3
"""Offline retrieval evaluation for AgentAtlas (never in the online path).

Inputs:
  --cases   tests/fixtures/evaluation/retrieval_cases.jsonl
  --results JSON file: {"<case_id>": ["doc_id_1", "doc_id_2", ...]} — the doc
            ids actually returned by the retrieval service for each query
            (produce it with any harness that calls POST /v1/retrieval or the
            retrieval package directly).

Metrics: hit-rate@k, MRR@k, and forbidden-doc violations (tenant isolation).
Exit code 1 when any forbidden doc appears or hit-rate < --min-hit-rate.

Optional deeper evaluation (faithfulness/answer relevance) runs in the
RAGAS/DeepEval sidecar — see README.md in this directory.
"""

import argparse
import json
import sys
from pathlib import Path


def load_cases(path: Path):
    with path.open(encoding="utf-8") as f:
        return [json.loads(line) for line in f if line.strip()]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--cases", required=True, type=Path)
    ap.add_argument("--results", required=True, type=Path)
    ap.add_argument("--min-hit-rate", type=float, default=0.6)
    args = ap.parse_args()

    cases = load_cases(args.cases)
    results = json.loads(args.results.read_text(encoding="utf-8"))

    hits, evaluable, rr_sum, violations = 0, 0, 0.0, []
    for case in cases:
        returned = results.get(case["case_id"], [])
        k = case.get("k", 10)
        topk = returned[:k]

        for forbidden in case.get("forbidden_doc_ids", []):
            if forbidden in topk:
                violations.append((case["case_id"], forbidden))

        relevant = case.get("relevant_doc_ids")
        if not relevant:
            continue
        evaluable += 1
        rank = next((i + 1 for i, d in enumerate(topk) if d in relevant), None)
        if rank:
            hits += 1
            rr_sum += 1.0 / rank

    hit_rate = hits / evaluable if evaluable else 0.0
    mrr = rr_sum / evaluable if evaluable else 0.0
    print(f"cases={len(cases)} evaluable={evaluable} hit_rate={hit_rate:.3f} mrr={mrr:.3f}")
    for case_id, doc in violations:
        print(f"VIOLATION tenant-isolation: case={case_id} leaked={doc}")

    if violations:
        return 1
    if evaluable and hit_rate < args.min_hit_rate:
        print(f"FAIL hit_rate {hit_rate:.3f} < min {args.min_hit_rate}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
