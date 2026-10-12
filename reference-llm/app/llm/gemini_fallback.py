# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Gemini model fallback: try gemini-2.5-flash first, fall back to gemini-3.5-flash.

Single choke point wrapping ``traced_complete`` for every Gemini call site
(generate_content, llm_classify, the theme classifier). When
``features.GEMINI_MODEL_FALLBACK_ENABLED`` is on, a call is attempted against
``llm.GEMINI_PRIMARY_MODEL`` and, ONLY on an availability-class failure
(``LLMUnavailableError`` — transport / timeout / 5xx / model-not-found — or a
``LLMRateLimitError``), retried against ``llm.GEMINI_FALLBACK_MODEL``. Deterministic
faults (safety block, input-too-large, a MAX_TOKENS truncation, auth) are NOT retried:
a different model would fail identically, so they propagate from the primary attempt.

Both models are standard Gemini Developer-API flash models on the same key/endpoint,
so the fallback is a model-string swap on the same provider — the provider itself
sends the per-model config (2.x: temperature + thinking_budget; 3.x: thinking_level).

Latency stays within each task's existing budget with NO Go-side timeout changes: the
two attempts share the budget. The primary runs against the full budget; the fallback
gets only what remains after the primary. Because the eligible triggers fail fast, the
primary normally either succeeds well under budget or errors in ~1s (leaving ~the full
budget for the fallback); only a degenerate primary *hang* eats the budget, in which
case the fallback is skipped rather than overrunning the deadline.

``max_total_budget_s`` lets a caller that already wraps this in an ``asyncio.wait_for``
declare that tighter ceiling, so the budget math and the skip-fallback floor reason
against the REAL deadline rather than the task's larger request timeout. Without it a
wrapped call would think it had the full task budget and launch a fallback the outer
wait_for then kills mid-flight. (No call site passes it today — every request runs on
its task's own timeout.)

When the flag is off this is a transparent pass-through to ``traced_complete`` using
the model the request already carries — today's single-model behavior, unchanged.
"""

from __future__ import annotations

import time

import structlog

from app.config.config import get_config
from app.llm.exceptions import LLMRateLimitError, LLMUnavailableError
from app.llm.providers.base import LLMProvider
from app.llm.tracing import traced_complete
from app.llm.types import CompletionRequest, CompletionResponse
from app.metrics import (
    GEMINI_FALLBACK_RESULT_FALLBACK,
    GEMINI_FALLBACK_RESULT_FALLBACK_ERROR,
    GEMINI_FALLBACK_RESULT_NO_BUDGET,
    GEMINI_FALLBACK_RESULT_PRIMARY,
    GEMINI_MODEL_FALLBACK_TOTAL,
)

logger = structlog.get_logger(__name__)


async def complete_with_gemini_fallback(
    provider: LLMProvider,
    request: CompletionRequest,
    *,
    generation_name: str | None = None,
    max_total_budget_s: float | None = None,
    disable_fallback: bool = False,
) -> CompletionResponse:
    """Run a Gemini completion with 2.5→3.5 model fallback (when enabled).

    ``generation_name`` is the caller's base Langfuse generation name (e.g.
    ``"gemini-classify"``); each attempt appends its model id so both are legible
    as distinct generations. When fallback is disabled the name is passed through
    to ``traced_complete`` unchanged. ``max_total_budget_s`` caps the two-attempt
    budget (see module docstring) — pass it when this runs inside an outer timeout.

    ``disable_fallback`` (per-task opt-out, TaskConfig.disable_gemini_fallback)
    forces the single-model pass-through even when the global flag is on: the call
    runs one attempt on the model the request carries (the task's pinned model),
    NOT the GEMINI_PRIMARY_MODEL override. Used by a task pinned to a model the
    fallback primary cannot serve, where the primary would fail deterministically.
    """
    config = get_config()

    if disable_fallback or not config.features.GEMINI_MODEL_FALLBACK_ENABLED:
        # Single-model pass-through: use the model the request already carries
        # (the task's pinned model when disable_fallback, else today's single-model
        # behavior when the global flag is off). Honor max_total_budget_s when a caller
        # declares a tighter ceiling, so the single attempt fails as a clean provider-timeout
        # at its own budget rather than being cancelled mid-flight by an outer wait_for.
        req = request
        if max_total_budget_s is not None:
            budget = request.timeout_seconds if request.timeout_seconds is not None else config.llm.REQUEST_TIMEOUT
            req = request.model_copy(update={"timeout_seconds": int(min(budget, max_total_budget_s))})
        return await traced_complete(provider, req, generation_name=generation_name)

    # A per-task pair (request.fallback_model set) overrides the global fleet pair, so a task
    # pinned to a different primary (e.g. the session-analysis producers on gemini-3.7-flash) falls
    # back to its own choice (gemini-2.5-flash) while the rest of the fleet keeps the global pair.
    if request.fallback_model is not None:
        primary_model = request.model
        fallback_model = request.fallback_model
    else:
        primary_model = config.llm.GEMINI_PRIMARY_MODEL
        fallback_model = config.llm.GEMINI_FALLBACK_MODEL
    task_type = request.task_type or "unknown"

    # Shared budget for the pair: the request's task-configured timeout (or the provider
    # default), clamped to any tighter ceiling the caller declares. The primary runs
    # against the full budget; the fallback gets the remainder (see module docstring).
    total_budget_s: float = (
        request.timeout_seconds if request.timeout_seconds is not None else config.llm.REQUEST_TIMEOUT
    )
    if max_total_budget_s is not None:
        total_budget_s = min(total_budget_s, max_total_budget_s)

    def _named(base: str | None, model: str) -> str:
        return f"{base}:{model}" if base else model

    primary_req = request.model_copy(update={"model": primary_model, "timeout_seconds": int(total_budget_s)})
    start = time.perf_counter()
    try:
        resp = await traced_complete(
            provider,
            primary_req,
            generation_name=_named(generation_name, primary_model),
            generation_metadata={"model_attempt": "primary"},
        )
        GEMINI_MODEL_FALLBACK_TOTAL.labels(task_type, GEMINI_FALLBACK_RESULT_PRIMARY).inc()
        return resp
    except (LLMUnavailableError, LLMRateLimitError) as exc:
        elapsed = time.perf_counter() - start
        remaining_s = total_budget_s - elapsed
        if remaining_s < config.llm.GEMINI_FALLBACK_MIN_BUDGET_SECONDS:
            # Primary consumed the budget (a hang, not a fast failure); a fallback
            # call can't finish within the ceiling, so surface the primary error.
            GEMINI_MODEL_FALLBACK_TOTAL.labels(task_type, GEMINI_FALLBACK_RESULT_NO_BUDGET).inc()
            logger.warning(
                "primary Gemini model failed with no budget left for fallback; surfacing primary error",
                task_type=task_type,
                primary_model=primary_model,
                fallback_model=fallback_model,
                elapsed_s=round(elapsed, 1),
                total_budget_s=total_budget_s,
                error=str(exc),
            )
            raise

        logger.warning(
            "primary Gemini model unavailable; falling back to secondary model",
            task_type=task_type,
            primary_model=primary_model,
            fallback_model=fallback_model,
            remaining_budget_s=round(remaining_s, 1),
            error=str(exc),
        )
        fallback_req = request.model_copy(update={"model": fallback_model, "timeout_seconds": int(remaining_s)})
        try:
            resp = await traced_complete(
                provider,
                fallback_req,
                generation_name=_named(generation_name, fallback_model),
                generation_metadata={"model_attempt": "fallback"},
            )
        except Exception:
            # Both models unavailable — count it so the metric captures the
            # both-down case, not just fallback successes; then propagate.
            GEMINI_MODEL_FALLBACK_TOTAL.labels(task_type, GEMINI_FALLBACK_RESULT_FALLBACK_ERROR).inc()
            raise
        GEMINI_MODEL_FALLBACK_TOTAL.labels(task_type, GEMINI_FALLBACK_RESULT_FALLBACK).inc()
        return resp
