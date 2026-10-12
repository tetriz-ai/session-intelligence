# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Google Gemini provider via the google-genai SDK."""

from __future__ import annotations

import asyncio
import base64
import binascii
import hashlib
import re
import time
from http import HTTPStatus
from typing import NoReturn

import httpx
import structlog
from google import genai
from google.genai import types
from google.genai.errors import ClientError, ServerError

from app.config.config import get_config
from app.llm.exceptions import (
    LLMAuthenticationError,
    LLMError,
    LLMInputTooLargeError,
    LLMInvalidRequestError,
    LLMOutputTruncatedError,
    LLMRateLimitError,
    LLMSafetyBlockedError,
    LLMUnavailableError,
)
from app.llm.prompt_cache import credit_request_cache_storage_cost, get_prompt_cache
from app.llm.providers.base import LLMProvider
from app.llm.types import (
    CompletionRequest,
    CompletionResponse,
    ConversationMessage,
    ConversationRole,
    TokenUsage,
    ToolCall,
)

logger = structlog.get_logger(__name__)

GEMINI_PROVIDER_NAME = "gemini"
GEMINI_DEFAULT_MODEL = "gemini-3.5-flash"

# Transient faults on the reference (generate_content) call that a bounded retry can clear —
# the same availability-class shapes prompt_cache retries create/list on. The session-analysis
# producers disable model fallback, so without this a single transient 5xx/transport error
# fails the whole session (replaying the all-or-nothing chunk chain next tick). The retry is
# gated on the caching intent (those producers) and re-bounded to the remaining request budget
# every attempt, so it never pushes a call past the caller's deadline.
_TRANSIENT_REFERENCE_EXC = (ServerError, httpx.TransportError, httpx.TimeoutException)
_REFERENCE_RETRIES = 2
# Don't start a reference retry, or a cache resolution, with less than this much of the request
# budget left — a call that cannot plausibly finish only burns the remainder of the deadline.
_MIN_BUDGET_SECONDS = 1.0

# Major version out of a Gemini model id, after lowercasing — matches "gemini-2..." /
# "gemini-3..." anywhere in the string, so a "models/" prefix or a suffix (flash, -preview,
# -latest) doesn't defeat it. Used by _uses_thinking_level to pick per-model config.
_GEMINI_MAJOR_VERSION_RE = re.compile(r"gemini-(\d+)")

# Finish reasons that mean the model stopped because content was blocked (vs merely
# incomplete). These map to LLMSafetyBlockedError — the same type the caller already
# counts as safety_blocked — so a no-text safety stop is classified by finish_reason
# rather than relying on the .text accessor raising ValueError. Everything else that
# is neither STOP nor unset is "incomplete" and maps to a generic LLMError.
_SAFETY_FINISH_REASONS = frozenset(
    {
        types.FinishReason.SAFETY,
        types.FinishReason.PROHIBITED_CONTENT,
        types.FinishReason.SPII,
        types.FinishReason.BLOCKLIST,
        types.FinishReason.IMAGE_SAFETY,
    }
)
# A clean completion ends on STOP (or the unset sentinel on responses that don't
# populate the field); any other finish reason is incomplete or blocked output.
_CLEAN_FINISH_REASONS = frozenset({None, types.FinishReason.FINISH_REASON_UNSPECIFIED, types.FinishReason.STOP})


def _function_call_part(call: ToolCall) -> types.Part:
    """One model-authored function call, rebuilt as the vendor part it came from.

    The thought signature is re-attached here rather than dropped. Gemini 3.x stamps a
    function call with an opaque signature and requires it back when that turn is replayed
    as history; a rebuilt call that omits it loses the model's reasoning across the round
    trip. A signature that is not valid base64 is dropped rather than raised on: it is an
    optimisation the vendor owns, and failing a customer's turn over an unreadable one
    trades a degraded answer for no answer.
    """
    part = types.Part.from_function_call(name=call.name, args=call.args)
    # The vendor's own id, back on the call it belongs to. It is optional, but once the
    # vendor populates one the result MUST come back carrying the matching id — and this
    # design runs independent calls concurrently, so a round whose ids are dropped is a
    # round whose results the model can only match by name. Two calls to the same tool then
    # become indistinguishable, which is a wrong answer rather than a failed one.
    if call.id and part.function_call is not None:
        part.function_call.id = call.id
    if call.thought_signature:
        try:
            part.thought_signature = base64.b64decode(call.thought_signature, validate=True)
        except (binascii.Error, ValueError):
            logger.warning("dropping unreadable tool-call thought signature", tool=call.name)
    return part


def _to_contents(conversation: list[ConversationMessage]) -> list[types.Content]:
    """Translate our conversation into the vendor's content list.

    Two translations happen here so no caller has to know them. A TOOL message becomes a
    `user` content, because that is the role Gemini carries function responses under — our
    own role exists so a caller never has to encode a tool result as something a person
    said. And all of one round's results stay in the ONE content they arrived in, never
    split, because splitting them degrades parallel tool calling on later turns.

    A message that carries nothing the vendor can represent is skipped rather than sent as
    an empty content, which the API rejects.
    """
    contents: list[types.Content] = []
    for message in conversation:
        parts: list[types.Part] = []
        if message.text:
            parts.append(types.Part.from_text(text=message.text))
        for call in message.tool_calls:
            parts.append(_function_call_part(call))
        for result in message.tool_results:
            part = types.Part.from_function_response(name=result.name, response=result.response)
            # The id of the call this answers — see _function_call_part for why it matters.
            if result.id and part.function_response is not None:
                part.function_response.id = result.id
            parts.append(part)
        if not parts:
            continue
        role = "model" if message.role == ConversationRole.MODEL else "user"
        contents.append(types.Content(role=role, parts=parts))
    return contents


def _tool_calls_from(response: types.GenerateContentResponse) -> list[ToolCall]:
    """The tool calls in a response, with the signature each one must be replayed with.

    Read off the parts rather than the SDK's `function_calls` accessor, because the
    signature lives on the PART and the accessor yields only the call inside it — and the
    signature is what the next request has to carry back.
    """
    candidates = response.candidates or []
    if not candidates or candidates[0].content is None:
        return []
    calls: list[ToolCall] = []
    for part in candidates[0].content.parts or []:
        call = part.function_call
        if call is None or not call.name:
            continue
        signature = part.thought_signature
        calls.append(
            ToolCall(
                name=call.name,
                args=call.args or {},
                id=call.id,
                thought_signature=base64.b64encode(signature).decode() if signature else None,
            )
        )
    return calls


class GeminiProvider(LLMProvider):
    def __init__(self, api_key: str, timeout: int = 120) -> None:
        self._client = genai.Client(api_key=api_key)
        self._timeout_ms = timeout * 1000
        # A stable, non-secret digest of the api_key: the system-prompt cache is a process-global
        # store shared across all per-tenant providers, and a Gemini CachedContent resource name is
        # only valid for the key that created it — so the cache key is scoped by this digest, never
        # by org, so two orgs on the SAME key share the cache and two on DIFFERENT keys never collide.
        self._cache_scope = hashlib.sha256(api_key.encode()).hexdigest()[:16]

    @property
    def cache_scope(self) -> str:
        return self._cache_scope

    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        # Cross-task contract: a MAX_TOKENS finish is treated as a hard LLMError for
        # EVERY task (see below), not just structured ones — there is no per-task
        # opt-out today. A truncated response is unusable, so callers get an error/502
        # rather than a silently-partial success; size each task's max_output_tokens
        # with headroom accordingly. If a future task legitimately wants best-effort
        # partial output, add an opt-out on CompletionRequest rather than special-casing.
        #
        # A per-request override (CompletionRequest.timeout_seconds) wins over the
        # provider default; None falls back to the constructor's REQUEST_TIMEOUT.
        timeout_ms = request.timeout_seconds * 1000 if request.timeout_seconds is not None else self._timeout_ms
        # The whole request shares ONE budget: cache resolution, then the generation, then any
        # reference retry all draw down from this deadline, so a slow cache service cannot consume
        # the caller's time and then still start a full-length generation on a fresh clock.
        deadline = time.monotonic() + timeout_ms / 1000.0
        config = types.GenerateContentConfig(
            http_options=types.HttpOptions(timeout=timeout_ms),
        )
        if request.max_tokens is not None:
            config.max_output_tokens = request.max_tokens
        # System-prompt caching: opt-in per call, Gemini-only, system prompt only.
        # When engaged, the system instruction rides a shared explicit CachedContent
        # (referenced by name) instead of being sent inline — so it is NOT set as
        # system_instruction (the two are mutually exclusive). Caching is an optimization,
        # never a correctness requirement: if the cache cannot be resolved (or resolution
        # outruns the request budget), degrade to the inline system prompt (uncached) rather
        # than failing a mandatory call.
        cached = False
        # The one-time storage cost of a cache this call CREATES; stays 0.0 on a cache
        # hit, when caching is off, or when resolution degrades to inline. Surfaced on the
        # response so the endpoint folds it into cost_usd.
        cache_storage_cost_usd = 0.0
        if request.use_system_prompt_cache and request.system_prompt:
            remaining_s = deadline - time.monotonic()
            if remaining_s <= _MIN_BUDGET_SECONDS:
                logger.warning(
                    "no request budget left for system-prompt cache resolution; sending inline (uncached)",
                    model=request.model,
                    task_type=request.task_type,
                )
            else:
                try:
                    # Bound resolution (caches.list + up to 3 caches.create) by the budget the
                    # caller has LEFT, so a slow cache service fails over to inline here rather
                    # than eating the deadline the generation still needs.
                    resolution = await asyncio.wait_for(
                        get_prompt_cache().resolve(
                            self._client.aio.caches,
                            task_type=request.task_type or "unknown",
                            model=request.model,
                            system_prompt=request.system_prompt,
                            ttl_seconds=get_config().llm.SYSTEM_PROMPT_CACHE_TTL_SECONDS,
                            cache_scope=self._cache_scope,
                        ),
                        timeout=remaining_s,
                    )
                    config.cached_content = resolution.name
                    cached = True
                    cache_storage_cost_usd = resolution.storage_cost_usd
                    # Credit the create's storage cost in THIS frame — the request handler's own
                    # context — not inside resolve(): asyncio.wait_for runs resolve() in a task
                    # with a copied context on the deployed Python 3.11, so a credit there never
                    # reaches the request tally. Crediting here, before generation, bills a cache
                    # create even when this attempt's generation later fails or is discarded by the
                    # 2.5→3.5 fallback. A cache hit carries 0.0, so this is a no-op for hits.
                    credit_request_cache_storage_cost(cache_storage_cost_usd)
                except Exception as exc:  # noqa: BLE001 — best-effort; incl. the wait_for TimeoutError
                    logger.warning(
                        "system-prompt cache resolution failed or timed out; sending the prompt inline (uncached)",
                        model=request.model,
                        task_type=request.task_type,
                        error=str(exc),
                    )
            # Re-bound the reference call to whatever budget survived resolution, so the generation
            # runs on the caller's real remaining deadline rather than the full task timeout. Floor
            # at 1s: a slow cache resolution can leave near-zero budget, and passing 0 ms would read
            # as "no timeout" (unbounded) to the SDK — the retry loop and the Go caller's HTTP
            # deadline still cap the total, so a bounded 1s first attempt is the safe minimum.
            config.http_options = types.HttpOptions(timeout=max(1000, int((deadline - time.monotonic()) * 1000)))
        if request.system_prompt and not cached:
            config.system_instruction = request.system_prompt
        if request.response_json_schema is not None:
            config.response_mime_type = "application/json"
            config.response_schema = request.response_json_schema

        # Native function declarations. The parameter schema passes through as
        # parameters_json_schema — plain JSON Schema, the same shape response_json_schema
        # already takes — so a tool is described the way a structured response is.
        #
        # Automatic function calling is DISABLED explicitly. The SDK can otherwise run a
        # declared function and loop on the result itself, and every part of that loop is
        # owned by the Go service by design: the turn state, the execution rules, the
        # round-trip counter and the identity a call is authorised against all live there.
        # This service answers one question per call — given these tools and this
        # conversation, what next — and a provider that looped would answer a different one.
        if request.tools:
            config.tools = [
                types.Tool(
                    function_declarations=[
                        types.FunctionDeclaration(
                            name=tool.name,
                            description=tool.description,
                            parameters_json_schema=tool.parameters_json_schema,
                        )
                        for tool in request.tools
                    ]
                )
            ]
            config.tool_config = types.ToolConfig(
                function_calling_config=types.FunctionCallingConfig(
                    mode=types.FunctionCallingConfigMode(request.tool_choice.value)
                )
            )
            config.automatic_function_calling = types.AutomaticFunctionCallingConfig(disable=True)

        # Sampling + thinking controls are model-generation-specific. The 2.5→3.5 model
        # fallback (see gemini_fallback.py) sends the SAME CompletionRequest to whichever
        # model it targets, so the provider — not the caller — picks the right knobs for
        # request.model: a 3.x model gets thinking_level and NO temperature (Google
        # recommends default sampling for 3.x reasoning), while a 2.x model gets the
        # pre-3.x knobs it understands (temperature + integer thinking_budget). Sending a
        # 2.x model thinking_level, or a 3.x model thinking_budget, is rejected by the API.
        if _uses_thinking_level(request.model):
            # gemini-3.x discrete thinking level (MINIMAL/LOW/MEDIUM/HIGH). None → model
            # default (MEDIUM). request.thinking_level is our ThinkingLevel enum; convert to
            # the SDK enum by value. temperature is intentionally omitted here.
            if request.thinking_level is not None:
                config.thinking_config = types.ThinkingConfig(
                    thinking_level=types.ThinkingLevel(request.thinking_level.value)
                )
            # A task may pin temperature on 3.x anyway (a stored-verdict classifier); the
            # request says so explicitly, so the default stays "omit".
            if request.pin_temperature_on_3x and request.temperature is not None:
                config.temperature = request.temperature
        else:
            # gemini-2.x: temperature is honored, and thinking is bounded by an integer
            # thinking_budget (None → the model's default dynamic thinking). This mirrors
            # how these tasks were tuned before the 3.x upgrade.
            if request.temperature is not None:
                config.temperature = request.temperature
            if request.thinking_budget is not None:
                config.thinking_config = types.ThinkingConfig(thinking_budget=request.thinking_budget)

        # A conversation request sends the whole exchange; a single-shot task sends its one
        # string, exactly as before. The validator on CompletionRequest guarantees one of them.
        contents: list[types.Content] | str = (
            _to_contents(request.conversation) if request.conversation else request.user_content
        )

        response = await self._generate_with_retry(request, config, contents, deadline)

        # A MAX_TOKENS finish means the model hit max_output_tokens and the payload
        # is truncated. For a structured (JSON-schema) task the partial usually fails
        # downstream Pydantic validation, but a truncation that happens to stay
        # parseable would otherwise ship as a silent success (e.g. a derive response
        # missing artifacts). Fail fast instead — the caller counts this as
        # llm_call_total{status=error} and returns 502 rather than acting on a partial
        # response. Applies to every task, not just derive.
        #
        # Check finish_reason BEFORE reading response.text: a MAX_TOKENS finish with no
        # text part makes the .text accessor raise ValueError, which the safety branch
        # below would otherwise misclassify as a safety block.
        candidates = response.candidates or []
        finish_reason = candidates[0].finish_reason if candidates else None

        # Build usage up front so it survives the raise paths below. A MAX_TOKENS
        # truncation or a safety block is still a billed generation — and MAX_TOKENS
        # is the peak-cost case (the model spent its whole budget thinking) — so the
        # caller must be able to record those tokens, not just successful replies.
        usage = None
        if response.usage_metadata:
            usage = TokenUsage(
                prompt_tokens=response.usage_metadata.prompt_token_count or 0,
                completion_tokens=response.usage_metadata.candidates_token_count or 0,
                total_tokens=response.usage_metadata.total_token_count or 0,
                # candidates_token_count is visible output only; thinking tokens bill
                # at the output rate but live here, so capture them separately for cost
                # observability. thoughts_token_count is None on non-thinking replies.
                thoughts_tokens=response.usage_metadata.thoughts_token_count or 0,
                # System-prompt tokens served from the explicit cache. A SUBSET of
                # prompt_token_count (Gemini counts cached input inside it), recorded so
                # pricing bills it at the cached rate. None/0 when caching didn't engage;
                # getattr guards an older SDK/usage shape that omits the field entirely.
                cache_read_tokens=getattr(response.usage_metadata, "cached_content_token_count", 0) or 0,
            )

        if finish_reason == types.FinishReason.MAX_TOKENS:
            logger.warning(
                "Gemini response truncated at max_output_tokens; raising LLMError",
                model=request.model,
            )
            raise LLMOutputTruncatedError(
                "Gemini response truncated at max_output_tokens",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=request.model,
            )
        # Any non-clean finish reason (not STOP, not unset, MAX_TOKENS already handled)
        # means the payload is incomplete or blocked. Fail fast — a partial-but-parseable
        # body (e.g. RECITATION / OTHER) must not ship as a silent success. A
        # content-block reason raises the safety error (so the caller counts it as
        # safety_blocked, matching the no-text ValueError path below); the rest raise a
        # generic LLMError. Checked before response.text so a no-text block is classified
        # by finish_reason, not by the accessor happening to raise.
        if finish_reason is not None and finish_reason not in _CLEAN_FINISH_REASONS:
            if finish_reason in _SAFETY_FINISH_REASONS:
                logger.warning(
                    "Gemini response blocked; raising LLMSafetyBlockedError",
                    model=request.model,
                    finish_reason=finish_reason.name,
                )
                raise LLMSafetyBlockedError(
                    f"Response blocked by Gemini (finish_reason={finish_reason.name})",
                    provider=GEMINI_PROVIDER_NAME,
                    usage=usage,
                    model=request.model,
                )
            logger.warning(
                "Gemini response incomplete; raising LLMError",
                model=request.model,
                finish_reason=finish_reason.name,
            )
            raise LLMError(
                f"Gemini response incomplete (finish_reason={finish_reason.name})",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=request.model,
            )

        # Only a request that declared tools can be answered with a call, so the single-shot
        # path never walks the parts looking for one.
        tool_calls = _tool_calls_from(response) if request.tools else []

        try:
            content = response.text or ""
        except ValueError:
            # The .text accessor raises when the reply carries no text part, which for a
            # prose-only task means the content was blocked. It does NOT mean that when the
            # model called a tool: a turn that asks for a read and says nothing alongside it
            # is the ordinary shape of tool calling, and treating it as a block would throw
            # away the call the whole turn exists to produce.
            if tool_calls:
                content = ""
                return CompletionResponse(
                    content=content,
                    model=request.model,
                    provider=GEMINI_PROVIDER_NAME,
                    usage=usage,
                    tool_calls=tool_calls,
                    cache_storage_cost_usd=cache_storage_cost_usd,
                    system_prompt_cache_referenced=cached,
                )
            logger.warning(
                "Gemini response blocked by safety filters; raising LLMSafetyBlockedError",
                model=request.model,
            )
            # Caller (generate_content.py) emits the safety_blocked counter and the
            # token usage via its except-clause so we don't double-count.
            raise LLMSafetyBlockedError(
                "Response blocked by safety filters",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=request.model,
            )

        return CompletionResponse(
            content=content,
            model=request.model,
            provider=GEMINI_PROVIDER_NAME,
            usage=usage,
            tool_calls=tool_calls,
            cache_storage_cost_usd=cache_storage_cost_usd,
            # True only when an explicit CachedContent was attached above (cached); False when
            # caching was off or resolution degraded to the inline system prompt.
            system_prompt_cache_referenced=cached,
        )

    async def _generate_with_retry(
        self,
        request: CompletionRequest,
        config: types.GenerateContentConfig,
        contents: list[types.Content] | str,
        deadline: float,
    ) -> types.GenerateContentResponse:
        """Run the Gemini generate_content reference call, retrying a transient fault.

        The retry is enabled only for the caching (session-analysis) producers. It is the FIRST of
        two recovery layers for a transient 5xx/transport blip: this same-model retry keeps the
        request's warm CachedContent and re-runs against the same model; only when it is exhausted
        does the outer complete_with_gemini_fallback swap to the task's availability fallback (a
        sustained outage of the primary). The two layers compose — a hard primary outage runs all
        of these attempts and THEN the fallback's attempts — but every attempt here is re-bounded
        to the request's REMAINING budget and skipped once too little remains, so the pair still
        never overruns the caller's deadline. Non-transient faults, and the input-too-large case,
        propagate on the first attempt as before.
        """
        attempts = _REFERENCE_RETRIES + 1 if request.use_system_prompt_cache else 1
        last_exc: Exception | None = None
        for attempt in range(attempts):
            if attempt > 0:
                remaining_s = deadline - time.monotonic()
                if remaining_s <= _MIN_BUDGET_SECONDS:
                    break
                config.http_options = types.HttpOptions(timeout=int(remaining_s * 1000))
            try:
                return await self._client.aio.models.generate_content(
                    model=request.model,
                    contents=contents,
                    config=config,
                )
            except Exception as exc:
                _handle_input_too_large(exc)  # raises for the input-too-large case; else returns
                last_exc = exc
                if attempt < attempts - 1 and isinstance(exc, _TRANSIENT_REFERENCE_EXC):
                    logger.warning(
                        "transient error on Gemini reference call; retrying",
                        model=request.model,
                        task_type=request.task_type,
                        attempt=attempt + 1,
                        error=str(exc),
                    )
                    continue
                _raise_llm_error(exc)  # NoReturn
        # Reached only when the budget ran out before a retry could run — surface the last fault.
        _raise_llm_error(last_exc)  # type: ignore[arg-type]  # last_exc is set on every break path

    async def close(self) -> None:
        # google-genai 2.x exposes aclose() on the async client (there is no close());
        # calling close() raises AttributeError, which close_all() would swallow and
        # then leak the underlying httpx client on shutdown.
        await self._client.aio.aclose()


def _handle_input_too_large(exc: Exception) -> None:
    """Raise LLMInputTooLargeError if the SDK reports an input-too-large error."""
    if isinstance(exc, ClientError):
        status = (exc.status or "").upper()
        if status == "INVALID_ARGUMENT" and "too long" in str(exc).lower():
            raise LLMInputTooLargeError(str(exc), provider=GEMINI_PROVIDER_NAME) from exc


def _uses_thinking_level(model: str) -> bool:
    """True for gemini-3.x+ models (thinking_level + no temperature); False for 2.x
    (temperature + thinking_budget).

    Extracts the major version from the model id, tolerant of the "models/" prefix and
    casing — both forms Google's API/SDK returns — e.g. "models/gemini-2.5-flash" → 2,
    "gemini-3.5-flash" → 3. A name with no gemini-<major> (an unversioned alias like
    "gemini-flash-latest", or a non-Gemini id) defaults to the 3.x style so a future
    3.x+ model is never sent the 2.x-only thinking_budget; a 2.x model MUST therefore be
    configured with an explicit versioned id, or it would be mis-detected as 3.x and the
    API would reject thinking_level with a 400 (config guidance lives on GEMINI_PRIMARY_MODEL).
    """
    match = _GEMINI_MAJOR_VERSION_RE.search((model or "").lower())
    if match is None:
        return True
    return int(match.group(1)) >= 3


def _raise_llm_error(exc: Exception) -> NoReturn:
    """Map google-genai SDK exceptions to our LLM error hierarchy.

    Availability-class failures (5xx, model-not-found/404, transport, timeout) map to
    LLMUnavailableError so the 2.5→3.5 model fallback retries only on failures a
    different model could survive; deterministic faults stay generic LLMError.
    """
    msg = str(exc)
    if isinstance(exc, ClientError):
        status = (exc.status or "").upper()
        if exc.code in (HTTPStatus.UNAUTHORIZED, HTTPStatus.FORBIDDEN) or status == "PERMISSION_DENIED":
            raise LLMAuthenticationError(msg, provider=GEMINI_PROVIDER_NAME) from exc
        if exc.code == HTTPStatus.TOO_MANY_REQUESTS or status == "RESOURCE_EXHAUSTED":
            raise LLMRateLimitError(msg, provider=GEMINI_PROVIDER_NAME) from exc
        if exc.code == HTTPStatus.NOT_FOUND or status in ("NOT_FOUND", "UNAVAILABLE"):
            # Model id unknown / not enabled for this key, or endpoint unavailable —
            # a different model may still serve, so this is fallback-eligible.
            raise LLMUnavailableError(msg, provider=GEMINI_PROVIDER_NAME) from exc
        if exc.code == HTTPStatus.BAD_REQUEST or status == "INVALID_ARGUMENT":
            # Non-input-size INVALID_ARGUMENT — the "too long" case is caught upstream by
            # _handle_input_too_large. See LLMInvalidRequestError for why it is non-retryable.
            raise LLMInvalidRequestError(msg, provider=GEMINI_PROVIDER_NAME) from exc
    # 5xx from the API, or a transport/connection/timeout error before any response.
    # (asyncio.TimeoutError is an alias of the builtin TimeoutError on Python 3.11+.)
    if isinstance(exc, (ServerError, httpx.TransportError, httpx.TimeoutException, ConnectionError, TimeoutError)):
        raise LLMUnavailableError(msg, provider=GEMINI_PROVIDER_NAME) from exc
    raise LLMError(msg, provider=GEMINI_PROVIDER_NAME) from exc
