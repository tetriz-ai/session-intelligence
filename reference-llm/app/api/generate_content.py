# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
Content generation endpoint.

The engine calls this to request LLM-processed content.
task_type is required — it determines the system prompt and default model config.
"""

import re
import time
from dataclasses import asdict

import structlog
from fastapi import APIRouter, Depends, HTTPException
from langfuse import get_client, observe
from structlog.contextvars import bind_contextvars

from app.config.config import get_config
from app.llm import tracing
from app.llm.exceptions import (
    LLMAuthenticationError,
    LLMError,
    LLMInputTooLargeError,
    LLMInvalidRequestError,
    LLMOutputTruncatedError,
    LLMRateLimitError,
    LLMSafetyBlockedError,
)
from app.llm.gemini_fallback import complete_with_gemini_fallback
from app.llm.pricing import compute_cost_usd, normalize_model_label
from app.llm.prompt_cache import (
    content_key,
    fold_cache_storage_cost_usd,
    request_cache_storage_cost_usd,
    reset_request_cache_storage_cost,
)
from app.llm.prompts.developer_profile import format_developer_profile_addendum
from app.llm.prompts.loops import (
    DELEGATED_SESSION_ADDENDUM,
    LOOP_ANALYSIS_ADDENDUM,
    LOOPS_ONLY_ADDENDUM,
    format_loop_input_block,
    format_loop_scope_addendum,
)
from app.llm.prompts.session_context import format_session_context_addendum
from app.llm.providers.provider_factory import ProviderFactory, get_provider_factory
from app.llm.router import (
    ANALYZE_LOOPS_TASK_TYPE,
    ANALYZE_PROMPT_TASK_TYPE,
    CHARS_PER_TOKEN,
    SESSION_ANALYSIS_TASK_TYPES,
    SESSION_TRACE_GROUPING_TASK_TYPES,
    TaskConfig,
    get_task_config,
)
from app.llm.schemas import (
    AnalyzePromptResponse,
)
from app.llm.trace_redaction import trace_input_requires_redaction, withheld_content_marker
from app.llm.types import CompletionRequest, CompletionResponse
from app.metrics import (
    LLM_CALL_DURATION,
    LLM_CALL_TOTAL,
    LLM_STATUS_AUTH_ERROR,
    LLM_STATUS_ERROR,
    LLM_STATUS_INPUT_TOO_LARGE,
    LLM_STATUS_INVALID_REQUEST,
    LLM_STATUS_OUTPUT_TRUNCATED,
    LLM_STATUS_RATE_LIMITED,
    LLM_STATUS_SAFETY_BLOCKED,
    LLM_STATUS_SUCCESS,
    MIXED_MODEL_ATTRIBUTION_TOTAL,
    VALIDATOR_RESULT_BLOCKING,
    VALIDATOR_RESULT_CLEAN,
    VALIDATOR_TOTAL,
    record_cache_storage_cost,
    record_token_usage,
)
from app.models.generate_content import (
    GenerateContentRequest,
    GenerateContentResponse,
    InsightsMetadataResponse,
    ValidationErrorResponse,
)
from app.tasks.analyze_prompt.loop_mode import (
    MODE_LOOPS_ONLY,
    is_delegated_session,
    requested_mode,
    response_schema_for,
    stamp_loop_contract_version,
    validate_blocks,
    wants_loops,
    wants_quality,
)
from app.tasks.analyze_prompt.session_limit_dedup import deduplicate_session_limit_count
from app.tasks.analyze_prompt.validators import validate_analyze_prompt_output

logger = structlog.get_logger(__name__)

router = APIRouter(prefix="/api/v1", tags=["generate"])


# Tag bounds. A tag is a project-global, browsable facet that outlives the trace that
# minted it, and both list fields below are client-reported `repeated string` with no
# bound of their own — so the cap lives here, at the single choke point.
_MAX_TAG_VALUE_CHARS = 40
_MAX_TAGS_PER_FACET = 8
# Recorded in metadata rather than tags, so these bound payload size, not cardinality.
# A facet with many collapsed values is itself a noise signal, so the count of what was
# dropped is kept even though the values are not.
_MAX_COLLAPSED_RECORDED = 10
_MAX_COLLAPSED_VALUE_CHARS = 200

# `other` says a value was not fit to be a facet; `__truncated__` says clean values were
# dropped by the cap. Distinct signals — a client value must not be able to forge either.
_COLLAPSED = "other"
_TRUNCATED = "__truncated__"
_RESERVED_TAG_VALUES = frozenset({_COLLAPSED, _TRUNCATED})

# A promoted value must look like an identifier. Client-reported model and language
# strings are free text, so anything else collapses to "other" rather than minting a
# customer-specific facet in a store every org's traces share.
# fullmatch, not match: `$` also matches before a trailing newline, which would let
# "x" and "x\n" mint two facets, one carrying a control character.
_SAFE_TAG_VALUE = re.compile(rf"[A-Za-z0-9._+#-]{{1,{_MAX_TAG_VALUE_CHARS}}}")


def _safe_tag_value(value: object) -> str:
    text = str(value).strip()
    if text in _RESERVED_TAG_VALUES:
        return _COLLAPSED
    return text if _SAFE_TAG_VALUE.fullmatch(text) else _COLLAPSED


def _context_strs(ctx: dict, *keys: str) -> list[str]:
    """Values under the first spelling that carries any — camelCase or snake_case.

    An empty value falls through to the next spelling rather than ending the search, so
    a client sending both does not lose the populated one. A value of an unexpected type
    is kept as a single string rather than dropped, so it collapses to `other` and stays
    visible instead of reading as "the session reported none".
    """
    for k in keys:
        v = ctx.get(k)
        if isinstance(v, list):
            if v:
                return [str(x) for x in v if x]
            continue
        if v:
            return [str(v)]
    return []


def _facet(prefix: str, values: list[str]) -> list[str]:
    """Bounded, de-duplicated tags for one multi-valued facet.

    Two distinct signals, so a partial facet never reads as a complete one: `other`
    means at least one value was unfit to be a facet, `__truncated__` means clean
    values were dropped by the cap. The values behind an `other` are recorded in trace
    metadata (`collapsed_tag_values`), which does not carry a tag's cardinality cost;
    filter on `:other`, then read them there.
    """
    mapped = list(dict.fromkeys(_safe_tag_value(v) for v in values))
    safe = [v for v in mapped if v != _COLLAPSED]
    collapsed_any = _COLLAPSED in mapped
    # Reserve the `other` slot BEFORE capping. It is one bounded value, so keeping it
    # costs no cardinality — and without the reservation a facet whose clean values fill
    # the cap would record raw values in metadata that no `:other` filter can reach.
    room = _MAX_TAGS_PER_FACET - (1 if collapsed_any else 0)
    tags = [f"{prefix}:{v}" for v in safe[:room]]
    if collapsed_any:
        tags.append(f"{prefix}:{_COLLAPSED}")
    if len(safe) > room:
        tags.append(f"{prefix}:{_TRUNCATED}")
    return tags


def _collapsed_tag_values(request: GenerateContentRequest) -> dict[str, dict[str, object]]:
    """Raw session-context values that did NOT survive the tag shape filter.

    Metadata, not tags: a value too odd to be a global facet stays inspectable on its own
    trace. `total` is kept even when the values are capped, because a facet with many
    collapsed values is a signal about the source, not just a bigger payload.
    """
    ctx = request.session_context or {}
    collapsed: dict[str, dict[str, object]] = {}
    for prefix, keys in (
        ("session_model", ("modelsUsed", "models_used")),
        ("lang", ("languages",)),
        ("ide", ("ideType", "ide_type")),
    ):
        raw = _context_strs(ctx, *keys)
        dropped = [v for v in dict.fromkeys(raw) if _safe_tag_value(v) == _COLLAPSED]
        if dropped:
            collapsed[prefix] = {
                "values": [v[:_MAX_COLLAPSED_VALUE_CHARS] for v in dropped[:_MAX_COLLAPSED_RECORDED]],
                "total": len(dropped),
            }
    return collapsed


def _trace_tags(request: GenerateContentRequest, extra: list[str] | None = None) -> list[str]:
    """Build the COMPLETE Langfuse tag set for a request.

    Only bounded, non-customer-identifying facets are promoted. Session context that
    identifies a customer (projectName, filesModified, gitCommits) is never promoted:
    a tag is a wider surface than trace input — a browsable facet reachable without
    opening any single trace.
    """
    ctx = request.session_context or {}

    # task_type is stamped before it is validated against the task registry, so a request
    # that will 400 must not be able to mint a facet either.
    tags = [_safe_tag_value(request.task_type), f"has_context:{'true' if ctx else 'false'}"]

    # Readers accept camelCase and snake_case alike (see llm/prompts/session_context.py).
    ide = ctx.get("ideType") or ctx.get("ide_type")
    if ide:
        tags.append(f"ide:{_safe_tag_value(ide)}")

    # `session_model` is the model that produced the SESSION; `analyzed_by` (added at the
    # post-inference stamp) is the model that analysed it. Different questions.
    tags += _facet("session_model", _context_strs(ctx, "modelsUsed", "models_used"))
    tags += _facet("lang", _context_strs(ctx, "languages"))

    # Extras are built by call sites reading the same unvalidated request, so they pass
    # through the same guard — otherwise the choke point has a second, open input.
    for tag in extra or []:
        prefix, sep, value = tag.partition(":")
        tags.append(f"{prefix}:{_safe_tag_value(value)}" if sep else _safe_tag_value(prefix))
    return tags


def _set_outer_span_input(span_input: dict[str, object]) -> None:
    """Set the generate-content span's input, best-effort.

       The @observe decorator's auto-capture is off (capture_input=False), so this is the ONLY
       writer of the outer span input. That is the point: if this call raises, the span keeps NO
       input rather than falling back to a raw customer-content capture — which is what lets the
    redaction hold even when the langfuse call fails, without a mandatory session-analysis
       request failing because a facet did not land.
    """
    try:
        get_client().update_current_span(input=span_input)
    except Exception as exc:
        logger.warning("langfuse span-input set failed; continuing", error=str(exc))


@router.post("/generate-content", response_model=GenerateContentResponse)
# capture_input=False: the decorator does NOT auto-capture this function's raw arguments as the
# span input. The session-analysis producers and classify_loop_artifact carry customer-
# authored content in `request.content` that must never reach an exported trace input, and an
# auto-capture races those redactions — a best-effort overwrite that fails, or a formatting
# error before the overwrite, would leave the raw capture on the span to be flushed. With
# capture off, the raw request is never placed on the span in the first place, so each task
# sets its own span input below: the session-analysis producers set a length-only view, the
# classify branch sets its scrubbed prompt, and every other task restores the raw request
# explicitly (unchanged trace behaviour for them). A failure to set the safe view then leaves
# the span with NO input — never the raw one — so the boundary holds even if langfuse raises.
@observe(name="generate-content", capture_input=False)
async def generate_content(
    request: GenerateContentRequest,
    factory: ProviderFactory = Depends(get_provider_factory),
) -> GenerateContentResponse:
    # Bind request context to structlog so org_id, task_type, and source_id
    # appear in every log line (via merge_contextvars). source_id is whatever
    # identifier the caller has handy — raw_record_id for analyze_prompt,
    # issue_key for issue_quality_check.
    bind_contextvars(
        org_id=request.org_id,
        task_type=request.task_type,
        source_id=request.source_id,
    )
    # Zero the per-request explicit-cache storage tally: the provider credits it at cache-create
    # time so it survives a model fallback that discards the creating call's response.
    reset_request_cache_storage_cost()

    # Langfuse REPLACES tags and metadata wholesale on every update_current_trace — it
    # does not merge, so a stamp listing only what it just learned erases every earlier
    # facet. Accumulate, and always re-emit the full set.
    trace_facets: list[str] = []
    trace_meta: dict[str, object] = {
        "task_type": request.task_type,
        "content_length": len(request.content),
        "org_id": request.org_id,
        "source_id": request.source_id,
    }
    _collapsed = _collapsed_tag_values(request)
    if _collapsed:
        trace_meta["collapsed_tag_values"] = _collapsed

    # Langfuse session grouping: the session-analysis engine fans one session out to N
    # LLM calls (R, E, D, loops, and R's per-window chunk calls), so set session_id to the
    # engine's session identity (the raw_record_id, forwarded as source_id) — all N calls then
    # group under one session view. The grouping set includes analyze_loops (part of the same
    # session's fan-out) even though loops is NOT in the caching/redaction set. Other task types
    # set no session_id (each call is its own).
    _session_id = request.source_id if request.task_type in SESSION_TRACE_GROUPING_TASK_TYPES else None
    if request.task_type in SESSION_TRACE_GROUPING_TASK_TYPES:
        # chunk_index groups R's per-window chunk calls within the session. Forwarded by
        # Go on session_context when a session is chunked; absent (one-chunk sessions) → no tag.
        _ctx = request.session_context or {}
        _chunk_index = _ctx.get("chunkIndex", _ctx.get("chunk_index"))
        if _chunk_index is not None:
            trace_meta["chunk_index"] = _chunk_index
            trace_facets.append(f"chunk_index:{_chunk_index}")

    def _stamp_trace() -> None:
        # Telemetry is best-effort. update_current_trace delegates to span.update_trace()
        # and catches nothing, and one stamp sits inside the validator's try/except where a
        # raise would be swallowed as a validation crash — dropping VALIDATOR_TOTAL and the
        # response metadata. The response must never fail because a facet did not land.
        try:
            # session_id only when set — passing None would clear a session already stamped.
            extra_trace: dict = {"session_id": _session_id} if _session_id is not None else {}
            get_client().update_current_trace(
                tags=_trace_tags(request, trace_facets),
                # Snapshot: trace_meta keeps mutating after this call, and a reference
                # would make every recorded stamp read as the final one.
                metadata=dict(trace_meta),
                **extra_trace,
            )
        except Exception as exc:
            logger.warning("langfuse trace stamp failed; continuing", error=str(exc))

    _stamp_trace()

    try:
        task_config = get_task_config(request.task_type)
    except ValueError as exc:
        logger.warning(
            "rejected generate-content request: unknown task_type; returning 400",
            error=str(exc),
        )
        raise HTTPException(status_code=400, detail=str(exc)) from exc

    temperature = request.temperature if request.temperature is not None else task_config.temperature
    max_tokens = request.max_tokens if request.max_tokens is not None else task_config.max_tokens

    # Resolve the model the PRIMARY attempt runs against up front, so the pre-call input-size gate
    # and the in-call non-success/duration arms label the SAME model complete_with_gemini_fallback
    # actually runs. It is the global GEMINI_PRIMARY_MODEL ONLY when fallback is enabled AND this
    # task neither opted out (disable_gemini_fallback) nor pinned its own pair (fallback_model);
    # otherwise the request's configured model runs — flag off, opted out, OR a per-task pair (the
    # session-analysis producers on 3.7), whose task_config.model is the primary.
    config = get_config()
    primary_model = (
        config.llm.GEMINI_PRIMARY_MODEL
        if config.features.GEMINI_MODEL_FALLBACK_ENABLED
        and not task_config.disable_gemini_fallback
        and task_config.fallback_model is None
        else task_config.model
    )
    configured_model = normalize_model_label(primary_model)

    logger.info(
        "generate-content request received; dispatching to LLM",
        model=task_config.model,
        provider=task_config.provider,
        content_length=len(request.content),
    )

    provider = factory.get_provider(request.org_id)

    # Set this span's input explicitly — the @observe decorator's auto-capture is off (see the
    # decorator comment). By default the request content (session transcript + any tenant skill
    # bodies) is withheld and replaced by a length-only marker, so it can never reach an exported
    # trace input; if this set fails the span simply carries no input, never the raw request. Raw
    # content is placed on the span only when an operator opts into raw export AND the task is not
    # a session-analysis producer (those are always withheld) — trace_input_requires_redaction
    # encodes that policy, shared with the generation-span redaction.
    if trace_input_requires_redaction(request.task_type):
        _set_outer_span_input(
            {"task_type": request.task_type, "content": withheld_content_marker(len(request.content))}
        )
    else:
        _set_outer_span_input(request.model_dump(mode="json"))

    system_prompt = task_config.system_prompt
    loop_input: str | None = None
    # analyze_loops IS the loop contract, so the task type decides it and session_context
    # cannot widen or narrow it. Forcing the mode here is what makes every downstream
    # mode-driven branch — response validation, the quality-validation gate — correct for
    # this task without each one having to learn about it.
    if request.task_type == ANALYZE_LOOPS_TASK_TYPE:
        analysis_mode = MODE_LOOPS_ONLY
    else:
        analysis_mode = requested_mode(request.session_context)
    # Delegation is a fact the CALLER states, read from its own key. The analysis mode
    # cannot carry it: a loops-only request means the call answers the loop contract alone,
    # which is equally true of a human-driven session whose loops are analysed separately.
    # Inferring delegation from the mode would waive the autonomy bar for those sessions.
    delegated_child = is_delegated_session(request.session_context)
    response_json_schema = task_config.response_json_schema
    # Loops reach the model two ways. On analyze_loops the whole system prompt is the loop
    # contract. On analyze_prompt one call must answer both contracts, so the same contract
    # is grafted on as an addendum and the mode picks a schema that drops the quality fields
    # — guided decoding then cannot score a child's prompt quality even if the prompt were
    # misread. Both paths need the loop DATA below.
    loops_via_addendum = request.task_type == ANALYZE_PROMPT_TASK_TYPE and wants_loops(analysis_mode)
    if loops_via_addendum:
        response_json_schema = response_schema_for(analysis_mode)
        system_prompt = system_prompt + "\n\n" + LOOP_ANALYSIS_ADDENDUM
        if not wants_quality(analysis_mode):
            system_prompt = system_prompt + "\n\n" + LOOPS_ONLY_ADDENDUM
    if loops_via_addendum or request.task_type == ANALYZE_LOOPS_TASK_TYPE:
        # A child's `user:` turns are the orchestrating agent, so without this the autonomy
        # bar in the loop contract reads them as a human interrupting and the model omits the
        # run — before the server-side delegated waiver can admit it, because the run is never
        # reported at all. Both loop paths need it.
        if delegated_child:
            system_prompt = system_prompt + "\n\n" + DELEGATED_SESSION_ADDENDUM
        # Scope is an instruction, so it stays in the system prompt.
        loop_scope = format_loop_scope_addendum(request.session_context or {})
        if loop_scope:
            system_prompt = system_prompt + "\n\n" + loop_scope
        # The candidates and the event timeline MUST reach the model: the addendum above
        # tells it to cite `session_events` sequences and to preserve the `loop_candidates`
        # it was given. The generic session-context formatter is a fixed key allowlist and
        # never carries either, so without this the model is asked to reference a timeline
        # it was never shown and every locator it invents is rejected.
        #
        # They ride USER CONTENT, not the system prompt — see format_loop_input_block for
        # why (cacheability, and untrusted tool text out of the instruction role).
        loop_input = format_loop_input_block(request.session_context or {})

    if request.task_type == ANALYZE_PROMPT_TASK_TYPE and request.session_context is not None:
        try:
            addendum = format_session_context_addendum(request.session_context)
            if addendum:
                system_prompt = system_prompt + "\n\n" + addendum
        except Exception:
            logger.exception("session-context addendum failed; using base system prompt")
        try:
            profile_addendum = format_developer_profile_addendum(request.session_context)
            if profile_addendum:
                system_prompt = system_prompt + "\n\n" + profile_addendum
                _profile = (request.session_context or {}).get("developer_profile") or {}
                _persona = _profile.get("persona", "UNKNOWN")
                _tool_mode = _profile.get("tool_mode", "unknown")
                _confidence = _profile.get("persona_confidence", 0.0)
                logger.info(
                    "developer profile addendum applied",
                    persona=_persona,
                    tool_mode=_tool_mode,
                    persona_confidence=round(_confidence, 2),
                    system_prompt_chars=len(system_prompt),
                )
                trace_meta.update(
                    developer_persona=_persona,
                    developer_tool_mode=_tool_mode,
                    developer_persona_confidence=round(_confidence, 2),
                    has_developer_profile=True,
                )
                trace_facets += [f"persona:{_persona}", f"tool_mode:{_tool_mode}"]
                _stamp_trace()
            else:
                logger.info(
                    "developer profile addendum skipped — no signal or below threshold",
                    has_session_context=bool(request.session_context),
                )
        except Exception:
            logger.exception("developer-profile addendum failed; continuing without profile context")

    # Appended after the conversation, so the model reads the session first and the
    # timeline as reference material for it.
    user_content = request.content
    if loop_input:
        user_content = user_content + "\n\n" + loop_input

    # Gated HERE, on the ASSEMBLED prompt and content — not on task_config.system_prompt
    # before the addenda exist. The old placement measured the base prompt while up to two
    # LOOP_CONTEXT_MAX_CHARS blocks went out unseen, so the check passed on a request that
    # was not the one being sent.
    max_tokens = _validate_token_limits(
        task_config, system_prompt, user_content, max_tokens, request.task_type, configured_model
    )

    # System-prompt caching: the request flag is tri-state, so an explicit choice always
    # wins — a caller that sends False turns caching OFF even on a session-analysis producer (the
    # rollback path). Only when the flag is unset (None) does the per-task default apply: ON for
    # the session-analysis producers (the capability's first consumers, whose large authored
    # system prompts are stable across a day of cron ticks), OFF for everything else. Engages
    # only inside the Gemini provider (system prompt present, Gemini route).
    if request.use_system_prompt_cache is not None:
        use_system_prompt_cache = request.use_system_prompt_cache
    else:
        use_system_prompt_cache = request.task_type in SESSION_ANALYSIS_TASK_TYPES

    comp_req = CompletionRequest(
        model=task_config.model,
        fallback_model=task_config.fallback_model,
        system_prompt=system_prompt,
        user_content=user_content,
        temperature=temperature,
        max_tokens=max_tokens,
        response_json_schema=response_json_schema,
        task_type=request.task_type,
        timeout_seconds=task_config.request_timeout_seconds,
        thinking_level=task_config.thinking_level,
        thinking_budget=task_config.thinking_budget,
        use_system_prompt_cache=use_system_prompt_cache,
        pin_temperature_on_3x=task_config.pin_temperature_on_3x,
    )

    # Non-success and duration are labelled by the model the PRIMARY attempt actually
    # runs against (configured_model, resolved above) — the only model a failure can be
    # attributed to, since no served model exists on the error path. Success + token/cost
    # use the SERVED model (served_labels / exc.model) instead.
    llm_labels = (request.task_type, task_config.provider, configured_model)

    def _billed_labels(exc: LLMError) -> tuple[str, str, str]:
        # Token/cost labels for a billed rejection — attribute to the model that
        # produced the tokens (exc.model), falling back to the configured model when
        # the exception carries none (transport-class errors set no model).
        return (request.task_type, task_config.provider, normalize_model_label(exc.model or task_config.model))

    # The `try/finally` collapses the per-branch duration observation into
    # one site — guarantees the histogram is observed exactly once regardless
    # of which exception type fires (or success), and forecloses the bug of
    # a future handler forgetting to call `.observe()`. A coroutine so the router
    # below can either serve it directly (team/org, or routing off) or invoke it
    # as a bounded Gemini call under an outer deadline.
    async def _run_primary(max_budget: float | None = None, pinned_model: str | None = None) -> CompletionResponse:
        # max_budget: the real ceiling when this runs inside an outer wait_for (the
        # a bounded caller passes a smaller budget); None on the direct
        # path, where the task's own request timeout is the ceiling.
        #
        # pinned_model: run this attempt against exactly this model, with no 2.5→3.5
        # fallback. A multi-sample request pins every sample after the first to the model
        # that served the first, because the response reports ONE model beside the SUMMED
        # tokens and cost of every call it made (see the vote block).
        start = time.perf_counter()
        attempt_req = comp_req if pinned_model is None else comp_req.model_copy(update={"model": pinned_model})
        # Non-success and duration are labelled by the model THIS attempt runs against —
        # the pinned one when given, else the configured primary — since no served model
        # exists on the error path. Duration is relabelled to the SERVED model on success
        # (below) so a 2.5→3.5 fallback-served call's latency lands under the fallback
        # model, matching its success/token/cost labels.
        attempt_labels = (
            llm_labels
            if pinned_model is None
            else (request.task_type, task_config.provider, normalize_model_label(pinned_model))
        )
        duration_labels = attempt_labels
        try:
            try:
                resp = await complete_with_gemini_fallback(
                    provider,
                    attempt_req,
                    max_total_budget_s=max_budget,
                    # A pinned attempt must not fall back: a sample served by a model
                    # its siblings did not use would hide a second model's spend behind
                    # the single model the response names.
                    disable_fallback=task_config.disable_gemini_fallback or pinned_model is not None,
                )
                # `llm_call_total` carries ONE increment per provider call. A call that
                # FAILED carries its own failure kind, owned by the except arms below. A
                # call that RETURNED carries the outcome of the request it served, which is
                # not known yet here — a hard post-LLM normalize can still reject this
                # completion, and then the tokens bought nothing usable. So a returned call
                # is counted by whoever learns that outcome: the multi-sample block counts
                # every sample once the vote resolves, and every other task by the single
                # increment before the return.
                duration_labels = (request.task_type, task_config.provider, normalize_model_label(resp.model))
                return resp
            except LLMInputTooLargeError as exc:
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_INPUT_TOO_LARGE).inc()
                logger.warning(
                    "LLM input too large; returned 400 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                )
                raise HTTPException(status_code=400, detail=str(exc)) from exc
            except LLMInvalidRequestError as exc:
                # 400 (terminal), not the generic-LLMError 502 — see LLMInvalidRequestError.
                # error-level, unlike input_too_large's warning: a schema/param the model
                # rejects is our bug, not an expected caller condition.
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_INVALID_REQUEST).inc()
                logger.error(
                    "LLM rejected request as invalid (deterministic); returned 400 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                    error=str(exc),
                )
                raise HTTPException(status_code=400, detail=str(exc)) from exc
            except LLMRateLimitError as exc:
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_RATE_LIMITED).inc()
                logger.warning(
                    "LLM rate-limited; returned 429 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                )
                raise HTTPException(status_code=429, detail=str(exc)) from exc
            except LLMAuthenticationError as exc:
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_AUTH_ERROR).inc()
                logger.error(
                    "LLM authentication failed; returned 502 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                )
                raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
            except LLMSafetyBlockedError as exc:
                # Must precede `except LLMError` since it is a subclass; if it
                # falls through to the generic clause we'd double-count as
                # status=error.
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_SAFETY_BLOCKED).inc()
                # Billed generation — record its tokens/cost against the model that
                # actually produced them (exc.model), which under fallback may differ
                # from the model the attempt ran against.
                record_token_usage(_billed_labels(exc), exc.usage)
                logger.warning(
                    "LLM response blocked by safety filter; returned 502 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                )
                raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
            except LLMOutputTruncatedError as exc:
                # Must precede `except LLMError` since it is a subclass. Kept separate
                # because it is DETERMINISTIC: the same input truncates again at the same
                # billed cost, so a caller must be able to stop rather than retry, and a
                # dashboard must be able to tell it apart from a transient 5xx.
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_OUTPUT_TRUNCATED).inc()
                # Billed: the truncated generation and its thinking tokens are charged.
                record_token_usage(_billed_labels(exc), exc.usage)
                logger.warning(
                    "LLM response truncated at the output budget; returned 502 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                    # The split matters for the fix: thinking bills against the same
                    # ceiling, so a large budget leaves less room for the body.
                    completion_tokens=exc.usage.completion_tokens if exc.usage else None,
                    thoughts_tokens=exc.usage.thoughts_tokens if exc.usage else None,
                )
                raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
            except LLMError as exc:
                # Generic LLM failure that the provider didn't classify (network
                # error, HTTP 5xx, SDK parse failure, etc.).
                LLM_CALL_TOTAL.labels(*attempt_labels, LLM_STATUS_ERROR).inc()
                # A billed rejection is attributed to the producing model (exc.model),
                # which may differ from the configured one under fallback.
                record_token_usage(_billed_labels(exc), exc.usage)
                logger.error(
                    "LLM call failed; returned 502 to caller",
                    provider=task_config.provider,
                    task_type=request.task_type,
                    error=str(exc),
                )
                raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
        finally:
            LLM_CALL_DURATION.labels(*duration_labels).observe(time.perf_counter() - start)

    # The cache-STORAGE cost is credited (in the provider) at cache-create time, BEFORE generation.
    # If a create succeeds but the generation then fails, no 200 response reaches the success-path
    # fold below, so the tally would be dropped from cost accounting. The finally records it to the
    # Prometheus series on that failure path (the per-call cost event can't capture it — there is no
    # response); on success the fold at record_cache_storage_cost(...) below owns it (served labels
    # + the response-field max), so the finally is guarded to fire only when the call did NOT return.
    _llm_call_returned = False
    try:
        comp_resp = await _run_primary()
        served_by = task_config.provider
        _llm_call_returned = True
    finally:
        if not _llm_call_returned and request_cache_storage_cost_usd() > 0:
            record_cache_storage_cost(llm_labels, request_cache_storage_cost_usd())

    # Which model actually served the response, so a trace can be filtered by provider
    # and by analysing model.
    _analyzed_by = normalize_model_label(comp_resp.model)
    trace_meta.update(
        served_by=served_by,
        analyzed_by=_analyzed_by,
    )
    trace_facets += [
        f"provider:{served_by}",
        f"analyzed_by:{_analyzed_by}",
    ]
    # Caching signal: whether this call's system prompt was served from cache. Derived
    # from the served usage — cache_read_tokens > 0 means the explicit cache hit. cache_read
    # volume itself rides usage_details on the generation span (tracing._usage_details).
    if use_system_prompt_cache:
        _cache_hit = bool(comp_resp.usage and comp_resp.usage.cache_read_tokens > 0)
        trace_meta["cache_hit"] = _cache_hit
        trace_facets.append(f"cache_hit:{'true' if _cache_hit else 'false'}")
        # The resolved cache key, so a trace can be joined to the shared CachedContent object
        # the call actually referenced. Stamped ONLY when the provider attached an explicit cache
        # (system_prompt_cache_referenced — a create or a hit), and keyed by comp_resp.model (the
        # SERVED model), not task_config.model: on a model-fallback the served call caches under the
        # fallback model (a producer degraded from its 3.7 primary to 2.5), so keying by the
        # configured primary would name the wrong object. When caching was requested but resolution
        # degraded to an inline system prompt (a cache list/create outage, or no budget left), NO
        # cache object was referenced — a cache_key would point at one this call never used — so
        # stamp a degradation facet instead, keeping the uncached path legible rather than silently
        # mislabelled. Metadata, never a tag: a high-cardinality content hash, not a browsable facet.
        if system_prompt:
            if comp_resp.system_prompt_cache_referenced:
                trace_meta["cache_key"] = content_key(
                    request.task_type, comp_resp.model, system_prompt, provider.cache_scope
                )
            else:
                # There was a system prompt to cache but no cache object was referenced — resolution
                # degraded to the inline prompt. (No system prompt at all is neither a hit nor a
                # degrade, so it stamps nothing here.)
                trace_facets.append("cache_degraded_inline")
    _stamp_trace()

    # Token metrics are labelled by the model that ACTUALLY served the response
    # (served_by), so per-provider token/cost series stay accurate under routing
    # and fallback. `comp_resp.usage` is truthy even at all-zero counts, so gate
    # on total_tokens > 0 to avoid materializing a zero-valued Prometheus labelset.
    served_labels = (request.task_type, served_by, normalize_model_label(comp_resp.model))
    record_token_usage(served_labels, comp_resp.usage)
    # What this request BILLED, across every provider call it made — not just the one whose
    # content is returned. A voted task issues one call per sample and the vendor charges for
    # each, so reporting a single call's usage would understate the task's spend by the sample
    # count everywhere it is read. Later calls add to these where they are collected. Cost
    # accumulates per call at the rate card of the model THAT call was served by, so samples
    # split across the 2.5→3.5 fallback are each priced correctly.
    billed_usage = comp_resp.usage
    billed_cost_usd = compute_cost_usd(normalize_model_label(comp_resp.model), comp_resp.usage)
    # Fold in the one-time explicit-cache STORAGE cost this request incurred by CREATING a
    # system-prompt cache: Gemini bills storage per cached-token-hour separately from the
    # per-call token cost, so a cached session under-reports its true cost without it. The fold takes
    # the max of the request tally and the served response's field (see fold_cache_storage_cost_usd
    # for why max, and why each channel is the reliable one in a case the other misses). Added to both
    # the per-call cost event's cost_usd (here) and the Prometheus cost series (below) so the sinks agree.
    cache_storage_cost_usd = fold_cache_storage_cost_usd(
        request_cache_storage_cost_usd(), comp_resp.cache_storage_cost_usd
    )
    if cache_storage_cost_usd > 0:
        billed_cost_usd = (billed_cost_usd or 0.0) + cache_storage_cost_usd
        record_cache_storage_cost(served_labels, cache_storage_cost_usd)
    # Which models produced those tokens. The response reports ONE model, so a request whose
    # calls span models cannot attribute them all faithfully; this set is what turns that from
    # a silent corruption into a detectable one (see the check before the return).
    billed_models = {normalize_model_label(comp_resp.model)}

    insights_metadata: InsightsMetadataResponse | None = None
    result_content = comp_resp.content

    # Per-block validation is reported independently of the quality validator below,
    # so a malformed loop block is visible even when quality validated cleanly (and
    # vice versa). Go re-validates and owns the persistence decision; this is the
    # analyzer-side signal for whether the contract is drifting.
    if request.task_type in (ANALYZE_PROMPT_TASK_TYPE, ANALYZE_LOOPS_TASK_TYPE) and wants_loops(analysis_mode):
        try:
            quality_block, loop_block = validate_blocks(comp_resp.content, analysis_mode)
            if not quality_block.valid or not loop_block.valid:
                logger.warning(
                    "loop block validation found an invalid block",
                    task_type=request.task_type,
                    raw_record_id=request.source_id,
                    analysis_mode=analysis_mode,
                    quality_block_valid=quality_block.valid,
                    quality_block_error=quality_block.error,
                    loop_block_valid=loop_block.valid,
                    loop_block_error=loop_block.error,
                )
        except Exception:
            logger.exception("analyze_prompt_block_validation_crashed", org_id=request.org_id)

    # The quality validator parses the response AS AnalyzePromptResponse, so it only
    # applies when the mode actually asked for quality. A loops_only response has no
    # quality fields at all, and running it there would fail every child session.
    if (
        request.task_type == ANALYZE_PROMPT_TASK_TYPE
        and config.features.ANALYZE_PROMPT_VALIDATION
        and wants_quality(analysis_mode)
    ):
        try:
            parsed = AnalyzePromptResponse.model_validate_json(comp_resp.content)
            parsed = deduplicate_session_limit_count(parsed, request.content)
            # result_content is now schema-normalized and deduplicated JSON.
            # On the validation-off path it remains the raw LLM string (undeduplicated).
            result_content = parsed.model_dump_json()
            vr = validate_analyze_prompt_output(parsed)

            # Attach evaluator scores to the Langfuse trace.
            if tracing.is_enabled():
                for evaluator_id, score in vr.scores.items():
                    # Per-score, so one failing evaluator costs only its own score. Same
                    # hazard as _stamp_trace: inside the validator's try/except, an
                    # unguarded raise reads as a validation crash and drops both
                    # VALIDATOR_TOTAL and the response's metadata.
                    try:
                        get_client().score_current_trace(name=evaluator_id, value=float(score))
                    except Exception as exc:
                        logger.warning(
                            "langfuse score attach failed; continuing",
                            evaluator_id=evaluator_id,
                            error=str(exc),
                        )

            # Same constant the VALIDATOR_TOTAL metric is labelled with below, so the
            # trace facet and the metric can never disagree about one response.
            _validator_outcome = VALIDATOR_RESULT_BLOCKING if vr.has_blocking else VALIDATOR_RESULT_CLEAN
            trace_meta["validator_outcome"] = _validator_outcome
            trace_facets.append(f"validator:{_validator_outcome}")
            _stamp_trace()

            # "blocking" is the error SEVERITY, not request control flow: the
            # insight is always persisted (we never raise). Log says so plainly
            # so on-call doesn't read this as a blocked/failed request.
            # In this branch task_type == analyze_prompt, so source_id is the
            # raw_record_id; keep that key name in the log so dashboards/alerts
            # filtering on raw_record_id keep working.
            if vr.has_blocking:
                VALIDATOR_TOTAL.labels(VALIDATOR_RESULT_BLOCKING).inc()
                logger.error(
                    "analyze_prompt validation found blocking-severity errors (advisory only; "
                    "request not blocked, insight still persisted with metadata flagged)",
                    raw_record_id=request.source_id,
                    error_codes=[e.code for e in vr.errors if e.severity == "blocking"],
                    error_count=len(vr.errors),
                )
            else:
                VALIDATOR_TOTAL.labels(VALIDATOR_RESULT_CLEAN).inc()

            insights_metadata = InsightsMetadataResponse(
                errors=[ValidationErrorResponse(**asdict(e)) for e in vr.errors],
                has_blocking=vr.has_blocking,
                error_count=len(vr.errors),
            )
        except Exception:
            logger.exception("analyze_prompt_validation_crashed", org_id=request.org_id)
            # Fall back to the original LLM JSON on any failure.
            result_content = comp_resp.content
            insights_metadata = None

    # Stamp the loop contract onto every response that answers it — the standalone loop
    # task and both loop-carrying modes of analyze_prompt. The consumer refuses to persist a
    # snapshot whose response does not positively claim the contract it was produced under,
    # so an unstamped loop answer is a discarded one.
    #
    # It is stamped HERE, after the normalize branches above, and that placement is
    # load-bearing: the quality path re-serializes the envelope through
    # AnalyzePromptResponse, which drops every key the schema does not declare — a stamp
    # written before it would silently vanish on exactly the combined-mode calls.
    if request.task_type == ANALYZE_LOOPS_TASK_TYPE or loops_via_addendum:
        result_content = stamp_loop_contract_version(result_content)

    # INVARIANT: one request bills one model. The response reports a single `model` beside
    # the summed tokens and cost of every call it made, and the Go success hook turns that
    # pair into one analytics event — so a request spanning two models would emit a row whose
    # cost cannot be recomputed from its own model and token counts. Every path here upholds
    # it: a single-call task bills one call, and a multi-sample task pins its samples to the
    # model that served the first. This guard is what would catch a future path that does
    # not — a second provider tier in one request, say — with the counter proving whether any
    # such row exists in a window and the warning naming the source_id to join against it.
    if len(billed_models) > 1:
        MIXED_MODEL_ATTRIBUTION_TOTAL.labels(request.task_type).inc()
        logger.warning(
            "request billed across several models but reported under one; "
            "its analytics event over-attributes spend to the reported model",
            task_type=request.task_type,
            org_id=request.org_id,
            source_id=request.source_id,
            reported_model=normalize_model_label(comp_resp.model),
            billed_models=sorted(billed_models),
        )
        trace_meta.update(billed_models=sorted(billed_models), mixed_model_attribution=True)
        _stamp_trace()

    # The one provider call this request made returned AND every hard post-LLM normalize
    # passed — counted here rather than at the call site, which cannot yet know the second
    # half (see the deferral note there). Labelled by served_by (served_labels), not the
    # task's default provider, so the success lands on the model that actually served under
    # routing/fallback.
    LLM_CALL_TOTAL.labels(*served_labels, LLM_STATUS_SUCCESS).inc()
    return GenerateContentResponse(
        result=result_content,
        task_type=request.task_type,
        model=comp_resp.model,
        provider=comp_resp.provider,
        usage=billed_usage,
        # Same rate-card math as the Prometheus cost series, summed over the same calls, so
        # the cost metric and the per-call cost event agree. None when no call was priced
        # (a local or otherwise unpriced model).
        cost_usd=billed_cost_usd,
        metadata=insights_metadata,
    )


def _estimate_input_tokens(system_prompt: str, content: str) -> int:
    """Rough input-token estimate (~4 chars/token) for size gating. Used by both
    the model-limit pre-check."""
    return (len(system_prompt) + len(content)) // CHARS_PER_TOKEN


def _validate_token_limits(
    task_config: TaskConfig,
    system_prompt: str,
    content: str,
    max_tokens: int | None,
    task_type: str,
    model_label: str,
) -> int | None:
    """Pre-validate input size and clamp the output token limit before calling the LLM.

    Returns the (possibly clamped) max_tokens the caller must use. A caller-supplied
    max_tokens above the model's max_output_tokens is clamped down rather than passed
    through, so a client cannot drive unbounded output cost. Raises HTTPException(400)
    if the estimated input exceeds the model's input limit.
    """
    if task_config.max_input_tokens is not None:
        # The ASSEMBLED prompt, not task_config.system_prompt: the caller gates after the
        # addenda are appended, and the loop payload alone can add two 200k-char blocks.
        estimated_input_tokens = _estimate_input_tokens(system_prompt, content)
        if estimated_input_tokens > task_config.max_input_tokens:
            # Symmetry with Gemini-side LLMInputTooLargeError handling — both pre-call
            # and provider-reported input rejections land on the same
            # (task_type, provider, model, status=input_too_large) series. model_label is
            # the primary model the caller resolved, matching the in-call arm's llm_labels
            # so the two rejections agree on the `model` dimension (not just after sum by).
            LLM_CALL_TOTAL.labels(task_type, task_config.provider, model_label, LLM_STATUS_INPUT_TOO_LARGE).inc()
            logger.warning(
                "rejected generate-content request: estimated input tokens exceed model limit; returning 400",
                model=task_config.model,
                estimated_input_tokens=estimated_input_tokens,
                max_input_tokens=task_config.max_input_tokens,
                content_chars=len(content),
            )
            raise HTTPException(
                status_code=400,
                detail=(
                    f"Estimated input size (~{estimated_input_tokens} tokens) exceeds "
                    f"model limit ({task_config.max_input_tokens} tokens). Reduce content length."
                ),
            )

    if task_config.max_output_tokens is not None and max_tokens is not None:
        if max_tokens > task_config.max_output_tokens:
            logger.warning(
                "requested max_output_tokens exceeds model limit; capping to model max",
                model=task_config.model,
                requested=max_tokens,
                capped_to=task_config.max_output_tokens,
            )
            return task_config.max_output_tokens

    return max_tokens
