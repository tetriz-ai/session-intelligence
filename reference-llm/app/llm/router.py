# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Task-to-config routing.

Each task type defines a system prompt and default model parameters.
Go sends a task_type string; Python looks up the config here.

To add a new task:
  1. Add an entry to TASK_REGISTRY below
  2. That's it — the endpoint picks it up automatically
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from app.llm.prompts import (
    ANALYZE_DISCARD_SYSTEM,
    ANALYZE_EVIDENCE_SYSTEM,
    ANALYZE_PROMPT_SYSTEM,
    ANALYZE_READ_SYSTEM,
    LOOP_ANALYSIS_SYSTEM,
)
from app.llm.providers.gemini import GEMINI_DEFAULT_MODEL, GEMINI_PROVIDER_NAME
from app.llm.schemas import (
    AnalyzeDiscardResponse,
    AnalyzeEvidenceResponse,
    AnalyzeLoopsResponse,
    AnalyzePromptQualityResponse,
    AnalyzeReadResponse,
)
from app.llm.types import ThinkingLevel


@dataclass(frozen=True)
class TaskConfig:
    """Configuration for a single task type."""

    system_prompt: str
    provider: str = GEMINI_PROVIDER_NAME
    model: str = GEMINI_DEFAULT_MODEL
    # None → the temperature param is OMITTED from the request. gemini-3.x reasoning is
    # tuned for default sampling and Google recommends not sending temperature/top_p/top_k,
    # so a 3.x task should leave this None; older/non-3.x tasks may still set a float.
    temperature: float | None = 0.7
    max_tokens: int | None = None
    response_json_schema: dict[str, Any] | None = field(default=None)
    max_input_tokens: int | None = None
    max_output_tokens: int | None = None
    # Per-task override for the Gemini provider HTTP timeout, in seconds. None →
    # the provider's constructor-configured REQUEST_TIMEOUT applies. An override must
    # stay strictly below the task's Go-side HTTP client timeout so Python fails first;
    # the value and its budget rationale are set per task in TASK_REGISTRY.
    request_timeout_seconds: int | None = None
    # How many independent samples decide this task's answer. 1 means a single call, which
    # is right for anything whose output a human reads and re-reads.
    #
    # Above 1 the caller votes on the samples, and each one is a separately billed provider
    # call. Reach for it only when the same input MUST yield the same answer twice and a low
    # temperature does not deliver that on its own — a stored verdict that then governs every
    # later read, rather than a document a human re-reads. A low temperature is not zero, and
    # a borderline input sampled at a low temperature still answers differently between calls.
    self_consistency_samples: int = 1
    # Per-task Gemini thinking effort (ThinkingLevel enum: MINIMAL|LOW|MEDIUM|HIGH).
    # None → model default (MEDIUM). This is the gemini-3.x control, used when the call
    # targets a 3.x model.
    thinking_level: ThinkingLevel | None = None
    # Per-task Gemini 2.x thinking budget (integer tokens). Used when the call targets a
    # 2.x model (the gemini-2.5-flash primary under GEMINI_MODEL_FALLBACK_ENABLED). None →
    # the 2.x model's default (dynamic) thinking — how every task except weekly_digest ran
    # on 2.5 before the 3.x upgrade. Ignored on 3.x models (thinking_level applies there).
    thinking_budget: int | None = None
    # When True, this task opts OUT of the global 2.5→3.5 model fallback: the call runs a
    # single attempt on `model` (this task's pinned model) instead of being overridden to
    # GEMINI_PRIMARY_MODEL. Use for a task pinned to a model whose response schema the
    # fallback primary cannot serve, so the primary would fail deterministically (no
    # availability fallback triggers) — routing it directly to the pinned model is the fix.
    disable_gemini_fallback: bool = False
    # Per-task availability fallback pair. When set (the global fallback flag on and
    # disable_gemini_fallback False), the fallback runs THIS task's `model` as primary and
    # `fallback_model` as fallback, instead of the global GEMINI_PRIMARY_MODEL/GEMINI_FALLBACK_MODEL.
    # Use for a task pinned to a different primary than the fleet default (e.g. the session-analysis
    # producers on gemini-3.7-flash falling back to gemini-2.5-flash) while the rest of the fleet
    # keeps the global pair. None → the global pair applies (today's behaviour).
    fallback_model: str | None = None
    # When True, `temperature` is sent to a gemini-3.x model as well (the provider otherwise
    # omits it for 3.x). For a schema-bound classifier whose verdict is stored and governs
    # every later read, deterministic sampling is worth more than Google's default-sampling
    # guidance for open-ended generation; see CompletionRequest.pin_temperature_on_3x.
    pin_temperature_on_3x: bool = False


# Max chars allowed in request content — 3_000_000 chars ≈ 750k tokens, leaves ~25% headroom
# for the system prompt within Gemini 3.5 Flash's 1M token context limit. Update this if the
# model changes.
MAX_CONTENT_CHARS = 3_000_000

# Conservative estimate: ~4 characters per token for English text. Lives here beside the
# content ceiling, rather than in one endpoint, because every endpoint that gates a request
# against a task's max_input_tokens must measure it the same way — two endpoints disagreeing
# on the estimate would accept at one door what the other rejects.
CHARS_PER_TOKEN = 4

# Task type constants — use these instead of raw strings to avoid silent typos.
ANALYZE_PROMPT_TASK_TYPE = "analyze_prompt"
ANALYZE_LOOPS_TASK_TYPE = "analyze_loops"
# Session Analysis Engine producers. R = Calls 1+2, E = Calls 3+6, D = Call 5.
ANALYZE_READ_TASK_TYPE = "analyze_read"
ANALYZE_EVIDENCE_TASK_TYPE = "analyze_evidence"
ANALYZE_DISCARD_TASK_TYPE = "analyze_discard"

# The producers whose system prompts are the session-analysis caching capability's first
# consumers and whose trace input is redacted at the export boundary. The
# endpoint reads this set to opt these tasks into system-prompt caching and to group their
# N-per-session calls under one Langfuse session_id.
SESSION_ANALYSIS_TASK_TYPES: frozenset[str] = frozenset(
    {ANALYZE_READ_TASK_TYPE, ANALYZE_EVIDENCE_TASK_TYPE, ANALYZE_DISCARD_TASK_TYPE}
)

# The task types that share ONE Langfuse session view: the three session-analysis
# producers PLUS analyze_loops — one session's analysis fans out to R, E, D AND loops, so all
# four must group under the same session_id to read as one session. This is trace GROUPING
# only; it deliberately does NOT opt analyze_loops into system-prompt caching or the
# trace-input redaction, both of which stay keyed on SESSION_ANALYSIS_TASK_TYPES.
SESSION_TRACE_GROUPING_TASK_TYPES: frozenset[str] = SESSION_ANALYSIS_TASK_TYPES | {ANALYZE_LOOPS_TASK_TYPE}

TASK_REGISTRY: dict[str, TaskConfig] = {
    ANALYZE_PROMPT_TASK_TYPE: TaskConfig(
        system_prompt=ANALYZE_PROMPT_SYSTEM,
        temperature=0.3,
        # LOW thinking effort. The session-analysis task is the one generate_content task kept above the
        # MINIMAL floor: scoring a session against the 12-check taxonomy needs a little
        # reasoning to stay accurate, but not the gemini-3.x MEDIUM default. (gemini-3.5-flash
        # — the default model for every task here — supports minimal|low|medium|high, so the
        # MINIMAL other tasks use is a valid level, not a rejected one.)
        thinking_level=ThinkingLevel.LOW,
        # Fixed 2.x thinking budget. On the 2.5 primary, thinking is billed against the
        # output-token ceiling it shares with the answer; left dynamic, a hard prompt's
        # reasoning can run away and exhaust that ceiling, tripping the MAX_TOKENS finish
        # gemini.py surfaces as a hard LLMError. 16_384 is ~2x this task's observed typical
        # reasoning — enough headroom for normal calls, while capping the runaway tail that
        # was the dominant source of those errors. (Ignored on the 3.x fallback, which is
        # governed by thinking_level.)
        thinking_budget=16_384,
        # The quality contract alone; a call that also wants loops swaps in the combined
        # schema at request time (see response_schema_for).
        response_json_schema=AnalyzePromptQualityResponse.model_json_schema(),
        max_input_tokens=1_048_576,
        max_output_tokens=65_536,
        # analyze_prompt has a long Gemini prefill tail that the shared REQUEST_TIMEOUT
        # cut short. 285s is strictly under the session-analysis client's 300s Go timeout
        # (defaultLLMTimeout), so Python fails first — the Go caller never cancels a
        # live generation. If you change this budget, revisit
        # ReferenceLLMAnalyzePromptHighErrorRate — it pages on calls that fail by hitting
        # this budget (504 DEADLINE_EXCEEDED timeouts count as errors).
        request_timeout_seconds=285,
    ),
    ANALYZE_LOOPS_TASK_TYPE: TaskConfig(
        system_prompt=LOOP_ANALYSIS_SYSTEM,
        # Ceilings and timeout still match analyze_prompt, which is what loop analysis ran
        # under when it shared that task; the thinking budget no longer does (see below).
        # Temperature is 0.0 (greedy), NOT inherited from analyze_prompt: a session is
        # re-analyzed on the same input (the main pass plus the reconcile / rejection-retry
        # sweep), and a sampling draw let the same timeline flip between "one run" and "no
        # runs" across those calls — the later empty pass then overwrote a real detected run.
        # 0.0 removes the sampling draw and does reach the API (GEMINI_PRIMARY_MODEL is a 2.x
        # model, on whose path the provider sends temperature — same reasoning as
        # classify_loop_artifact below). It is greedy decoding, not a determinism guarantee on
        # a hosted model, so it reduces rather than eliminates the flip.
        temperature=0.0,
        thinking_level=ThinkingLevel.LOW,
        # This budget is what the ANSWER competes with, which is why it is small. Under
        # GEMINI_MODEL_FALLBACK_ENABLED the call is served by GEMINI_PRIMARY_MODEL
        # (gemini-2.5-flash), where thinking_level is ignored and this integer governs, and on
        # 2.x thinking bills against the same output ceiling the body must fit inside. At
        # 16_384 a quarter of that ceiling was spent before the body started — the dominant
        # shape of this task's truncations, which land within ~20 tokens of the ceiling with
        # thinking at its full budget. 4_096 returns ~12k tokens to the body, and cuts the
        # serial decode that runs ahead of it (the same request is ended server-side at ~283s
        # with a 504 returning nothing, upstream of request_timeout_seconds below, so
        # generating less is the only lever on that deadline too).
        #
        # Sized as 2x classify_harness_intent's 2_048 because this task reasons over a whole
        # timeline rather than one artifact. Whether it ever under-thinks a very long session
        # is a post-deploy detection watch, not a proven bound — the value it replaces was
        # inherited from analyze_prompt and never measured against loop detection.
        thinking_budget=4_096,
        # The loop block is the ONLY thing this task may return, enforced by guided
        # decoding rather than by an instruction — a prompt-quality field is unreachable
        # here even if the prompt were misread.
        response_json_schema=AnalyzeLoopsResponse.model_json_schema(),
        max_input_tokens=1_048_576,
        max_output_tokens=65_536,
        # Must stay strictly below the Go LLMClient's 300s defaultLLMTimeout so Python
        # fails first and the caller never cancels a live generation.
        request_timeout_seconds=285,
    ),
    # ─── Session Analysis Engine producers ───
    # All three are MANDATORY Gemini calls on gemini-3.7-flash (primary) with a per-task
    # gemini-2.5-flash availability fallback: 3.7 at thinking_level=LOW bills ~0 thinking, so R's
    # answer stays under the 65,536 output ceiling that 2.5's dynamic thinking used to exhaust on a
    # dense window (a MAX_TOKENS truncation that never reached JUDGED). Accepted tradeoff: a 3.7
    # availability outage now yields a 2.5-SERVED verdict that is stored and, per the reconcile
    # model (re-admission keys on FAILED, not on a successful row), never re-judged on 3.7 — so the
    # stored-verdict corpus is model-heterogeneous (a mostly-3.7 body with a 2.5 minority from
    # outage windows). temperature=0.2 + pin_temperature_on_3x hold WITHIN-model determinism; the
    # cross-model drift on the rare outage path is deliberately accepted (a judged record beats a
    # never-JUDGED one). System-prompt caching is engaged by the endpoint for these task types
    # — now on the 3.7 primary (and on the 2.5 fallback when it serves); a transient 5xx on
    # the cache reference call is retried inside the provider before the model fallback swaps. The
    # 2.5 fallback still carries the 2.x thinking budget + 65,536 ceiling, so a dense window DURING
    # a 3.7 outage can still truncate — 3.7-primary fixes the steady state, not the degraded path.
    # temperature and the thinking budgets below are provisional W defaults — the contract leaves them to
    # eval calibration against real sessions, so revisit them when the eval bars are measured.
    ANALYZE_READ_TASK_TYPE: TaskConfig(
        system_prompt=ANALYZE_READ_SYSTEM,
        model="gemini-3.7-flash",
        fallback_model="gemini-2.5-flash",  # availability-only fallback (rare); 3.7 is the primary
        # Judged analysis, kept mostly-deterministic so a session re-analysed on the same input
        # (reconcile / rejection-retry sweeps) does not flip its verdict between calls; 3.x omits
        # temperature by default, so pin it to hold 0.2 for these stored verdicts.
        temperature=0.2,
        pin_temperature_on_3x=True,
        thinking_level=ThinkingLevel.LOW,
        # On the 3.7 primary, thinking_level=LOW bills ~0 thinking tokens (measured), so R's
        # per-window answer (one row per turn + one per phase + the verdict) sits well under the
        # 65,536 output ceiling — a dense window under 2.5's dynamic thinking used to exhaust that
        # ceiling. thinking_budget below is the 2.x knob: inert on 3.7, it caps the 2.5 fallback.
        thinking_budget=8_192,
        response_json_schema=AnalyzeReadResponse.model_json_schema(),
        max_input_tokens=1_048_576,
        max_output_tokens=65_536,
        # Default the output budget to the declared ceiling: Go never sends max_tokens for these
        # producers, and an unset max_tokens omits max_output_tokens entirely, so the call would
        # otherwise run at Gemini's own default rather than R's ceiling. Safe to pin here (unlike
        # the clamp-only weekly_digest/derive tasks) because thinking is separately bounded
        # (thinking_level=LOW on the 3.7 primary bills ~0; thinking_budget caps the 2.5 fallback),
        # so a large answer budget cannot inflate the call's thinking/generation time.
        max_tokens=65_536,
        request_timeout_seconds=285,
    ),
    ANALYZE_EVIDENCE_TASK_TYPE: TaskConfig(
        system_prompt=ANALYZE_EVIDENCE_SYSTEM,
        model="gemini-3.7-flash",
        fallback_model="gemini-2.5-flash",  # availability-only fallback (rare); 3.7 is the primary
        temperature=0.2,
        pin_temperature_on_3x=True,  # 3.x omits temperature by default; hold 0.2 for stored verdicts
        thinking_level=ThinkingLevel.LOW,
        # thinking_budget is the 2.x knob: inert on the 3.7 primary (LOW bills ~0), it caps the 2.5
        # fallback. E adjudicates ledgers + excerpts, not a whole transcript, so it reasons over
        # less than R — a smaller budget. Provisional.
        thinking_budget=4_096,
        response_json_schema=AnalyzeEvidenceResponse.model_json_schema(),
        max_input_tokens=1_048_576,
        max_output_tokens=32_768,
        # Default the output budget to E's declared ceiling (see analyze_read; thinking is bounded
        # separately so pinning the answer budget is safe).
        max_tokens=32_768,
        request_timeout_seconds=285,
    ),
    ANALYZE_DISCARD_TASK_TYPE: TaskConfig(
        system_prompt=ANALYZE_DISCARD_SYSTEM,
        model="gemini-3.7-flash",
        fallback_model="gemini-2.5-flash",  # availability-only fallback (rare); 3.7 is the primary
        temperature=0.2,
        pin_temperature_on_3x=True,  # 3.x omits temperature by default; hold 0.2 for stored verdicts
        thinking_level=ThinkingLevel.LOW,
        # thinking_budget is the 2.x knob: inert on the 3.7 primary (LOW bills ~0), it caps the 2.5
        # fallback. D is abstain-biased and its usual answer is an empty list, so it needs the least
        # reasoning of the three; a small budget caps the runaway tail. Provisional.
        thinking_budget=2_048,
        response_json_schema=AnalyzeDiscardResponse.model_json_schema(),
        max_input_tokens=1_048_576,
        # One range list, each range a prompt_range + a <=25-word reason — a small output.
        max_output_tokens=8_192,
        # Default the output budget to D's declared ceiling (see analyze_read; thinking is bounded
        # separately so pinning the answer budget is safe).
        max_tokens=8_192,
        request_timeout_seconds=285,
    ),
}


def get_task_config(task_type: str) -> TaskConfig:
    """Look up config for a task_type. Raises ValueError if unknown."""
    if task_type not in TASK_REGISTRY:
        valid = list(TASK_REGISTRY.keys())
        raise ValueError(f"Unknown task_type: {task_type!r}. Valid: {valid}")
    return TASK_REGISTRY[task_type]
