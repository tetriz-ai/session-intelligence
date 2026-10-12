# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Per-model USD pricing for LLM cost observability.

Cost is never returned by the provider — it is always tokens x published rate. This
module is the single source of truth for those rates, reused by both the Prometheus
cost metric (app/metrics.py) and the per-call cost event (the cost_usd
returned on each response and forwarded by Go).

Rates are USD per 1M tokens, transcribed from Google's official Gemini API rate card
(https://ai.google.dev/gemini-api/docs/pricing). Thinking/reasoning tokens bill at the
OUTPUT rate on the current card; ModelPrice.thinking_per_1m is an optional override for
a model — or a future card — that prices thinking separately.

Only externally-billed models are listed. Gemma-shaped models and local
ML models (embeddings, SBERT) have no per-token bill and are intentionally ABSENT:
compute_cost_usd returns None for them, so no cost is emitted.

Scope: this models flat input/output (+ optional thinking) per-token rates, the reduced
cached-input read rate, and the explicit-cache STORAGE rate (per cached-token-hour). It does
NOT cover Gemini's batch-API (~50% off) or >200k-token long-context tiers — neither is used
today (2.5/3.5-flash, no batch). Adopting one of those would be a schema change to ModelPrice
+ compute_cost_usd, not just a data edit here.
"""

from __future__ import annotations

from typing import NamedTuple

from app.llm.types import TokenUsage

_PER_MILLION = 1_000_000


class ModelPrice(NamedTuple):
    input_per_1m: float
    output_per_1m: float
    # Thinking/reasoning tokens. None -> billed at the output rate (current Google
    # card for every Gemini model). Set only for a model that prices thinking
    # separately, so a rate-card split is a data change here, not a code change.
    thinking_per_1m: float | None = None
    # Cached input tokens (system-prompt caching,). None -> billed at the FULL input
    # rate (no discount assumed — never underprices a model whose cached rate we haven't
    # recorded). Set only for a model whose card publishes a reduced cached-input rate.
    cached_input_per_1m: float | None = None
    # Explicit-cache STORAGE rate: USD per 1M cached tokens per HOUR the CachedContent is kept
    # alive (Google bills this separately from the per-call cached-read tokens above, for the
    # whole time the object exists). None -> the model is not used with an explicit cache, so no
    # storage cost is attributed (compute_cache_storage_cost_usd returns None). Set only for a
    # model whose system prompts we cache explicitly (app/llm/prompt_cache.py).
    cache_storage_per_1m_hour: float | None = None

    @property
    def thinking_rate(self) -> float:
        return self.output_per_1m if self.thinking_per_1m is None else self.thinking_per_1m

    @property
    def cached_input_rate(self) -> float:
        return self.input_per_1m if self.cached_input_per_1m is None else self.cached_input_per_1m


# Keyed by the NORMALIZED model label (see normalize_model_label). Rates are USD per 1M
# tokens so the table reads directly against Google's rate card.
#
# PRICING REVIEW: last reviewed 2026-10. Re-verify these rates AND the model ids below
# against Google's live Gemini API rate card (https://ai.google.dev/gemini-api/docs/pricing)
# and model catalog (https://ai.google.dev/gemini-api/docs/models) at each review — a rate
# change or a renamed/retired model id makes compute_cost_usd silently under/over-report (an
# unknown id returns None → no cost, flagged by reference_llm_llm_unpriced_billed_calls_total).
MODEL_PRICING: dict[str, ModelPrice] = {
    # cached_input_per_1m ($0.03 / 1M, text tier — Google's live card) and cache_storage_per_1m_hour
    # ($1.00 / 1M cached tokens / hour) are 2.5-flash's explicit-cache rates. 2.5-flash is the
    # availability FALLBACK for the three session-analysis producers (primary is 3.7 below); a
    # fallback-served call still caches its system prompt, so both rates are load-bearing on the
    # degradation path — the cached-read rate is priced here, not left stale.
    "gemini-2.5-flash": ModelPrice(
        input_per_1m=0.30, output_per_1m=2.50, cached_input_per_1m=0.03, cache_storage_per_1m_hour=1.00
    ),
    "gemini-3.5-flash": ModelPrice(input_per_1m=1.50, output_per_1m=9.00),
    # generate_insights (org/team) AND the PRIMARY for the three session-analysis producers (R/E/D),
    # which cache their large stable system prompts on 3.7 — hence the explicit-cache rates.
    # Output rate includes thinking tokens (card default → thinking_rate = output rate). Introductory
    # rates through 2026-12-31; Google's card doubles them on 2027-01-01 (tokens → 1.50/7.50,
    # cached-input → 0.15, storage → 1.00) — update this entry then (a dated test guards the tokens).
    "gemini-3.7-flash": ModelPrice(
        input_per_1m=0.75, output_per_1m=3.75, cached_input_per_1m=0.075, cache_storage_per_1m_hour=0.50
    ),
    # Gemini-3-Pro — the LLM-as-judge evaluator model. Reasoning bills at output (card
    # default). Flat <=200k-token rate; the long-context tier is out of scope (see above).
    "gemini-3.1-pro-preview": ModelPrice(input_per_1m=2.00, output_per_1m=12.00),
}


def normalize_model_label(model: str) -> str:
    """Collapse a raw response model string into a stable, bounded label value.

    A leading "models/" (the form Google's SDK sometimes echoes) is stripped so it
    matches the bare config id in MODEL_PRICING; Gemini ids then pass through unchanged.
    A Gemma-shaped model label can arrive as an env-specific string (empty in
    development) — collapse anything Gemma-shaped to "gemma" so the label stays bounded
    and env-independent. An empty string becomes "unknown". Local ML model/version
    strings pass through as-is; they have no price entry, so they never incur cost.

    Normalizing here keeps the Prometheus `model` label and the cost event's `model`
    field consistent. Casing is preserved (not lowered) so both sinks carry the same
    value byte-for-byte.
    """
    normalized = (model or "").strip()
    if normalized.startswith("models/"):
        normalized = normalized[len("models/") :]
    if not normalized:
        return "unknown"
    if "gemma" in normalized.lower():
        return "gemma"
    return normalized


def compute_cost_usd(model: str, usage: TokenUsage | None) -> float | None:
    """USD cost of one call, or None when the model has no published price.

    None (not 0.0) signals "no cost to report" — no billed tokens, a no-vendor-bill
    model (Gemma-shaped, local ML), or an unrecognized model — so callers skip emitting a
    cost series/field rather than recording a misleading $0. Gating on total_tokens > 0
    matches record_token_usage, so the Prometheus cost series and the per-call cost
    event agree (a zero-token call reports no cost on either sink). Thinking tokens bill
    at the thinking rate (= output rate unless the model overrides it).
    """
    if usage is None or usage.total_tokens <= 0:
        return None
    price = MODEL_PRICING.get(normalize_model_label(model))
    if price is None:
        return None
    # Gemini counts cached tokens INSIDE prompt_token_count, so split prompt tokens into the
    # cached portion (billed at the reduced cached-input rate) and the rest (full input rate).
    # The min(...) clamps a provider that ever reported cache_read_tokens > prompt_tokens, so
    # uncached_input can never go negative.
    cached_input = min(usage.cache_read_tokens, usage.prompt_tokens)
    uncached_input = usage.prompt_tokens - cached_input
    return (
        uncached_input * price.input_per_1m
        + cached_input * price.cached_input_rate
        + usage.completion_tokens * price.output_per_1m
        + usage.thoughts_tokens * price.thinking_rate
    ) / _PER_MILLION


_SECONDS_PER_HOUR = 3600


def compute_cache_storage_cost_usd(model: str, cached_tokens: int, ttl_seconds: int) -> float | None:
    """USD storage cost of holding one explicit CachedContent alive for its TTL, or None.

    Gemini bills explicit-cache storage per cached-token-HOUR, separate from the per-call cached
    input tokens compute_cost_usd already prices. This is a one-time commitment attributed to the
    call that CREATES the cache: we never delete a cache early, so it lives exactly ttl_seconds and
    the whole commitment is billed regardless of how many calls later read it. A cache HIT (an
    already-live object, adopted or in-store) creates nothing and so has no storage cost.

    None when the model has no storage rate on the card, or when there is nothing to bill (no
    cached tokens, non-positive TTL) — the same "no cost to report" signal as compute_cost_usd, so
    the caller skips folding a spurious zero into the call's cost.
    """
    if cached_tokens <= 0 or ttl_seconds <= 0:
        return None
    price = MODEL_PRICING.get(normalize_model_label(model))
    if price is None or price.cache_storage_per_1m_hour is None:
        return None
    ttl_hours = ttl_seconds / _SECONDS_PER_HOUR
    return cached_tokens * ttl_hours * price.cache_storage_per_1m_hour / _PER_MILLION
