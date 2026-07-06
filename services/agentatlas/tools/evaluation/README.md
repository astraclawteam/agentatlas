# Offline quality evaluation

Evaluation NEVER runs in the online request path. Two tiers:

## 1. Built-in retrieval metrics (no dependencies)

```sh
python tools/evaluation/run_eval.py \
  --cases tests/fixtures/evaluation/retrieval_cases.jsonl \
  --results /tmp/retrieval_results.json
```

`retrieval_results.json` maps each `case_id` to the ordered doc ids the
retrieval service returned. The script reports hit-rate@k and MRR@k and fails
(exit 1) on any tenant-isolation violation (`forbidden_doc_ids` leaking into
the top-k) or a hit-rate below `--min-hit-rate`.

Parser fixtures (`parser_cases.jsonl`) declare structural expectations
(headings, block types, tiling overlap, monotonic timestamps) checked by the
Go tests in `internal/parsergateway` and `internal/artifacts`.

## 2. RAGAS / DeepEval sidecar (optional, offline only)

For faithfulness / answer-relevance scoring, run the evaluation sidecar in an
isolated environment (it needs model access via the OpenAI-compatible
endpoint):

```sh
pip install ragas deepeval
OPENAI_API_BASE=$LLMROUTER_BASE_URL OPENAI_API_KEY=$LLMROUTER_API_KEY \
  python -m ragas evaluate ...   # feed answer traces exported from /v1/traces
```

Wire it against exported Answer Traces (question summary, retrieved snippets,
answer) — never against raw enterprise originals.
