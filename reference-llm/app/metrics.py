# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Prometheus metrics for reference-llm.

HTTP-level metrics (request count, request duration, sizes) come from
`prometheus-fastapi-instrumentator` (wired in app/main.py). The collectors
defined here cover the service-specific signals:

  - per-(task_type, provider, model) LLM call rate + error breakdown
  - per-(task_type, provider, model) LLM call latency
  - per-(task_type, provider, model) token spend and USD cost
  - the Gemini 2.5→3.5 model-fallback outcomes
  - analyze_prompt validator outcomes
  - detect-round-trips (PAIRS detector) request outcomes

Cost is never returned by a provider — it is derived from per-model rates in
app/llm/pricing.py (see record_token_usage), computed once so every cost consumer
(the metric and the per-call cost event) agrees. Models with no vendor bill have no
rate entry → no cost.

org_id is intentionally NOT a label — it stays in structlog contextvars only.
Adding it would explode cardinality with tenant growth.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from prometheus_client import Counter, Histogram

from app.llm.pricing import compute_cost_usd
from app.llm.providers.gemini import GEMINI_PROVIDER_NAME

if TYPE_CHECKING:
    from app.llm.types import TokenUsage

# ── LLM call status label values ──────────────────────────────────────────
LLM_STATUS_SUCCESS = "success"
LLM_STATUS_ERROR = "error"
LLM_STATUS_RATE_LIMITED = "rate_limited"
LLM_STATUS_AUTH_ERROR = "auth_error"
LLM_STATUS_INPUT_TOO_LARGE = "input_too_large"
LLM_STATUS_INVALID_REQUEST = "invalid_request"
LLM_STATUS_SAFETY_BLOCKED = "safety_blocked"
# Deterministic: the model filled its output budget and the body was cut off. Separated
# from `error` because retrying it produces the same truncation at the same cost, so the
# two need telling apart when viewing the metrics.
LLM_STATUS_OUTPUT_TRUNCATED = "output_truncated"

LLM_TOKEN_KIND_PROMPT = "prompt"
LLM_TOKEN_KIND_COMPLETION = "completion"
# Gemini thinking-model reasoning tokens — billed at the output rate, separate
# from the visible completion tokens. See TokenUsage.thoughts_tokens.
LLM_TOKEN_KIND_THINKING = "thinking"

VALIDATOR_RESULT_CLEAN = "clean"
VALIDATOR_RESULT_BLOCKING = "blocking"

# The (task_type, provider, model) triple is shared by the call/latency/token/cost
# metrics so every model invocation is sliceable by model. `model` is the served
# model, normalized (normalize_model_label) → bounded. provider is "gemini".
#
# Grain: ONE increment per provider call. A request that issues N calls contributes N.
# A call that FAILED carries its own failure kind (rate_limited, auth_error,
# output_truncated, …). A call that RETURNED carries the outcome of the request it served
# — success when it delivered a usable answer, `error` when a completion the task's schema
# rejected bought nothing usable.
LLM_CALL_TOTAL = Counter(
    "reference_llm_llm_call_total",
    "Model completion calls by task, provider, model, and outcome status.",
    labelnames=("task_type", "provider", "model", "status"),
)

# Buckets span 0.5s–300s. Most Gemini Flash calls finish in 1–10s; analyze_prompt can
# spike past 120s. 300s upper bound matches the LLM HTTP client timeout.
LLM_CALL_DURATION = Histogram(
    "reference_llm_llm_call_duration_seconds",
    "End-to-end model completion latency by task, provider, and model.",
    labelnames=("task_type", "provider", "model"),
    buckets=(0.5, 1, 2, 5, 10, 30, 60, 120, 180, 300),
)

LLM_TOKENS = Counter(
    "reference_llm_llm_tokens_total",
    "LLM tokens consumed by task, provider, model, and kind (prompt, completion, thinking).",
    labelnames=("task_type", "provider", "model", "kind"),
)

# Estimated USD cost of externally-billed LLM calls, derived from per-model rates
# (app/llm/pricing.py). Only priced models (Gemini) emit here.
LLM_COST = Counter(
    "reference_llm_llm_cost_usd_total",
    "Estimated USD cost of LLM calls by task, provider, and model (Gemini only).",
    labelnames=("task_type", "provider", "model"),
)

# Billed Gemini calls whose served model has NO entry in pricing.MODEL_PRICING — cost
# is silently omitted (understating spend) until the SKU is added. A non-zero value
# means "add this model to MODEL_PRICING".
LLM_UNPRICED_BILLED_CALLS = Counter(
    "reference_llm_llm_unpriced_billed_calls_total",
    "Billed Gemini calls with no rate-card entry (missing from pricing.MODEL_PRICING).",
    labelnames=("task_type", "model"),
)


def record_token_usage(labels: tuple[str, str, str], usage: TokenUsage | None) -> None:
    """Emit prompt/completion/thinking token spend AND USD cost for a billed call.

    Shared by every provider-call site so ALL LLM token spend — including thinking tokens,
    and including truncated/blocked calls that are still billed — lands in one series.
    `usage` is truthy even at all-zero, so gate on total_tokens > 0 to avoid a zero-valued
    labelset. `labels` is (task_type, provider, model) with model already normalized.

    Cost is derived here from the per-model rate card (compute_cost_usd) so it is computed
    once and stays consistent with the per-call cost event. A model with no vendor bill
    returns None → its tokens are recorded but no cost series is materialized."""
    if not (usage and usage.total_tokens > 0):
        return
    LLM_TOKENS.labels(*labels, LLM_TOKEN_KIND_PROMPT).inc(usage.prompt_tokens)
    LLM_TOKENS.labels(*labels, LLM_TOKEN_KIND_COMPLETION).inc(usage.completion_tokens)
    LLM_TOKENS.labels(*labels, LLM_TOKEN_KIND_THINKING).inc(usage.thoughts_tokens)
    cost_usd = compute_cost_usd(labels[2], usage)
    if cost_usd is not None and cost_usd > 0:
        LLM_COST.labels(*labels).inc(cost_usd)
    elif cost_usd is None and labels[1] == GEMINI_PROVIDER_NAME:
        # Gemini billed real tokens (past the total_tokens > 0 gate) but the model has no
        # price entry — flag the missing SKU rather than silently reporting no cost.
        LLM_UNPRICED_BILLED_CALLS.labels(labels[0], labels[2]).inc()


def record_cache_storage_cost(labels: tuple[str, str, str], storage_cost_usd: float) -> None:
    """Add the one-time explicit-cache STORAGE cost of a call that created a system-prompt cache
    into the same LLM_COST series, so the Prometheus cost stays consistent with the per-call
    cost_usd. 0.0/absent on a cache hit or an unpriced model → no-op. `labels` is
    (task_type, provider, model), model already normalized."""
    if storage_cost_usd > 0:
        LLM_COST.labels(*labels).inc(storage_cost_usd)


VALIDATOR_TOTAL = Counter(
    "reference_llm_analyze_prompt_validator_total",
    "analyze_prompt response validator outcomes.",
    labelnames=("result",),
)

# 2.5→3.5 model fallback outcomes (features.GEMINI_MODEL_FALLBACK_ENABLED). result:
#   "primary"        — primary served, no fallback needed
#   "fallback"       — primary unavailable, fallback served
#   "fallback_error" — primary unavailable AND the fallback also failed
#   "no_budget"      — primary unavailable but no time left to attempt fallback
# The four results cover every fallback-triggering outcome. Low cardinality: task_types × 4.
GEMINI_MODEL_FALLBACK_TOTAL = Counter(
    "reference_llm_gemini_model_fallback_total",
    "Gemini primary / fallback outcomes per task.",
    labelnames=("task_type", "result"),
)
GEMINI_FALLBACK_RESULT_PRIMARY = "primary"
GEMINI_FALLBACK_RESULT_FALLBACK = "fallback"
GEMINI_FALLBACK_RESULT_FALLBACK_ERROR = "fallback_error"
GEMINI_FALLBACK_RESULT_NO_BUDGET = "no_budget"

# ── detect-round-trips (PAIRS detector: MiniLM + NLI + scaler/logreg) ───────
DETECT_ROUND_TRIPS_STATUS_SUCCESS = "success"
DETECT_ROUND_TRIPS_STATUS_ERROR = "error"

DETECT_ROUND_TRIPS_TOTAL = Counter(
    "reference_llm_detect_round_trips_total",
    "detect-round-trips (PAIRS detector) request outcomes by status.",
    labelnames=("status",),
)

DETECT_ROUND_TRIPS_PAIRS = Counter(
    "reference_llm_detect_round_trips_pairs_total",
    "Guard-valid pairs scored by the round-trip detector.",
)

# CPU-bound MiniLM + NLI encode over a batch of pairs (two transformers, N pairs),
# so the buckets run wide.
DETECT_ROUND_TRIPS_DURATION = Histogram(
    "reference_llm_detect_round_trips_duration_seconds",
    "Time to score a batch of guard-valid pairs (MiniLM + NLI + classifier).",
    buckets=(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30),
)


# A request whose provider calls were served by MORE THAN ONE model. The response carries a
# single `model` alongside the SUMMED tokens and cost, so such a request would emit one
# analytics event attributing every call's spend to one of its models — a row that cannot be
# recomputed from its own model and token counts. This is a guard on an invariant: steady
# state is 0 BY CONSTRUCTION, so any non-zero value is actionable.
MIXED_MODEL_ATTRIBUTION_TOTAL = Counter(
    "reference_llm_mixed_model_attribution_total",
    "Requests billed across several models but reported under one.",
    labelnames=("task_type",),
)
