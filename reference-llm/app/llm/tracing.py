# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Langfuse tracing integration.

Centralises all observability SDK calls so individual providers and endpoints
stay decoupled from the Langfuse library.  When Langfuse is disabled (default)
or auth fails at startup, all functions are safe no-ops — the @observe decorator
passes through and update_current_* calls silently do nothing.
"""

from __future__ import annotations

import structlog
from langfuse import Langfuse, observe

from app.llm.exceptions import LLMError
from app.llm.providers.base import LLMProvider
from app.llm.trace_redaction import redacted_trace_input, trace_output
from app.llm.types import CompletionRequest, CompletionResponse, TokenUsage

logger = structlog.get_logger(__name__)

_enabled = False
_client: Langfuse | None = None


def configure(*, public_key: str, secret_key: str, host: str) -> bool:
    """Configure and authenticate with Langfuse.

    Returns True on success.  On auth failure the SDK is explicitly disabled
    so @observe decorators become no-ops and no events are silently buffered.
    """
    global _enabled, _client

    client = Langfuse(
        public_key=public_key,
        secret_key=secret_key,
        host=host,
    )

    try:
        auth_ok = client.auth_check()
    except Exception as exc:
        logger.warning(
            "langfuse host unreachable at startup; LLM tracing disabled",
            host=host,
            error=str(exc),
        )
        client.shutdown()
        return False

    if auth_ok:
        _client = client
        _enabled = True
        logger.info("langfuse tracing initialized", host=host)
        return True

    logger.warning(
        "langfuse authentication failed at startup; LLM tracing disabled (check keys / stack health)",
        hint="Check LANGFUSE keys and that the Langfuse stack is running",
    )
    client.shutdown()
    return False


def configure_disabled() -> None:
    """Register a single tracing-disabled Langfuse client when observability is off.

    The langfuse SDK's @observe decorator and get_client() lazily auto-create a
    *keyless* client on first use when none is registered — which logs an
    "Authentication error: ...without public_key" warning and (because that client
    has no active span) a "No active span" error on every LLM call. Registering one
    client up front with tracing_enabled=False makes get_client() resolve to it
    (single registered instance), so @observe and update_current_* become true
    no-ops and neither message is emitted. Non-empty placeholder keys avoid the
    no-public-key warning; tracing_enabled=False means no auth_check / no network.
    """
    global _enabled, _client
    _enabled = False
    _client = None
    Langfuse(public_key="disabled", secret_key="disabled", tracing_enabled=False)


def shutdown() -> None:
    """Flush any pending Langfuse events.  Safe to call when disabled."""
    if not _enabled or _client is None:
        return
    _client.flush()


def is_enabled() -> bool:
    return _enabled


# capture_input=False: the decorator does NOT auto-capture this function's raw arguments (the
# provider and the full CompletionRequest, whose user_content carries the session transcript,
# tenant skill bodies and per-tenant catalogue for the session-analysis producers). Auto-capture
# would place that raw request on the generation span the instant this function is entered — before
# the update_current_generation below can overwrite it with the redacted view — so a redaction that
# raised, or an error before it ran, would leave the raw transcript on the (error) span to be
# flushed, defeating the export boundary. With capture off the raw request is never placed on
# the span at all; the update below is the ONLY writer of the input, setting the redacted view, and
# if it fails the span simply carries no input rather than the raw one. Mirrors the outer
# generate-content span (app/api/generate_content.py).
#
# capture_output=False for the matching reason on the way out, and it is what makes the
# explicit output below the real one: the decorator captures the RETURN VALUE when the function
# returns, which is AFTER the update inside it, so with auto-capture on it overwrites whatever
# that update set and the whole CompletionResponse is exported. The error path never had this
# problem — the function raises, so the decorator captures nothing and the explicit update
# stands. Turning it off makes success behave the way the error path already does.
@observe(as_type="generation", name="llm-completion", capture_input=False, capture_output=False)
async def traced_complete(
    provider: LLMProvider,
    request: CompletionRequest,
    *,
    generation_name: str | None = None,
    generation_metadata: dict | None = None,
) -> CompletionResponse:
    """Execute an LLM completion with Langfuse generation-level tracing.

    Records model parameters, input, output, token usage, and errors.
    When Langfuse is disabled the @observe decorator is a no-op and the
    update_current_* calls silently do nothing.

    generation_name overrides the default "llm-completion" observation name so
    parallel calls under one trace (e.g. a primary call vs. a shadow
    call) appear as distinct generations. generation_metadata attaches
    extra correlation fields (e.g. source_id, shadow=True) to that generation.
    """
    if _client is not None:
        # Only forward name/metadata when explicitly provided — passing None
        # could null out the decorator's default "llm-completion" name for the
        # primary (Gemini) callers that don't override it.
        extra: dict = {}
        if generation_name is not None:
            extra["name"] = generation_name
        # Stamp task_type on the observation, not just the trace tag: Langfuse
        # trace tags do not propagate down to observations, so observation-level
        # evaluators can only filter/trigger on per-observation metadata. Set it
        # after merging caller metadata so this discriminator can't be clobbered.
        metadata: dict = dict(generation_metadata) if generation_metadata is not None else {}
        if request.task_type:
            metadata["task_type"] = request.task_type
        if metadata:
            extra["metadata"] = metadata
        # Record only the params the request actually carries, so the trace reflects
        # the config that was sent: a task that omits temperature (gemini-3.x default
        # sampling → temperature=None) shows no temperature rather than a misleading
        # value, and thinking_level (the gemini-3.x reasoning control) is included.
        model_parameters: dict = {"max_tokens": request.max_tokens}
        if request.temperature is not None:
            model_parameters["temperature"] = request.temperature
        if request.thinking_level is not None:
            model_parameters["thinking_level"] = request.thinking_level
        # Redact the trace input at the export boundary: for the session-analysis producers
        # the user_content (transcript, skill bodies, per-tenant catalogue) is withheld before
        # this span exports, so it never reaches a Langfuse trace input. Every other
        # task keeps the raw input redacted_trace_input passes through unchanged. Because the
        # decorator's auto-capture is off (see above), this is the ONLY writer of the generation
        # input — set here before provider.complete, so both the success and the error span carry
        # the redacted view, never the raw request.
        _client.update_current_generation(
            model=request.model,
            model_parameters=model_parameters,
            input=redacted_trace_input(request),
            **extra,
        )

    try:
        response = await provider.complete(request)
    except Exception as exc:
        if _client is not None:
            # Do not export str(exc): a provider/validation error can quote request-derived
            # content (an offending snippet, a safety-block detail), which would leak to the
            # trace span even when the input itself is withheld. Record the error class (and the
            # LLMError code when present) — enough to triage, nothing request-derived.
            status_message = type(exc).__name__
            if isinstance(exc, LLMError) and getattr(exc, "error_code", ""):
                status_message = f"{type(exc).__name__}: {exc.error_code}"
            update: dict = {"level": "ERROR", "status_message": status_message}
            if isinstance(exc, LLMError):
                # Mirror the success-path output shape so the generation trace carries
                # model/provider/usage even on a failed call — the @observe decorator
                # captures the return value on success, but on error the function raises
                # and the decorator gets nothing, leaving Output undefined.
                output: dict = {"model": exc.model, "provider": exc.provider}
                if exc.usage is not None:
                    output["usage"] = exc.usage.model_dump()
                    update["usage_details"] = _usage_details(exc.usage)
                update["output"] = output
            _client.update_current_generation(**update)
        raise

    if _client is not None:
        _client.update_current_generation(
            # A single-shot task's output is its content, unchanged; a conversation's carries
            # its tool calls beside it. See trace_output — it is the one place the export
            # shape is decided.
            output=trace_output(request, response),
            usage_details=_usage_details(response.usage),
        )

    return response


def _usage_details(usage: TokenUsage | None) -> dict[str, int]:
    """Langfuse usage_details for one generation, billed or rejected."""
    return {
        "input": usage.prompt_tokens if usage else 0,
        # Visible completion tokens only. Thinking tokens are reported on their own
        # "thinking" key (below) rather than folded in here, so reasoning spend is
        # legible in the trace instead of hiding inside output.
        "output": usage.completion_tokens if usage else 0,
        # Gemini reasoning tokens as a distinct line. These bill at the OUTPUT rate,
        # so for Langfuse cost to stay correct the model's price config must define a
        # matching per-unit price for the "thinking" usage type (equal to output);
        # without it the cost view counts these tokens as free. input+output+thinking
        # == total. 0 for non-thinking replies and providers that don't report it.
        "thinking": usage.thoughts_tokens if usage else 0,
        # System-prompt tokens served from the explicit cache. A SUBSET of
        # "input" (Gemini counts cached input inside prompt_token_count), reported on its own
        # line so cache-read volume is legible per generation. "total" is passed explicitly
        # (input+output+thinking), so this annotation line does not inflate it.
        "cache_read": usage.cache_read_tokens if usage else 0,
        "total": usage.total_tokens if usage else 0,
    }
