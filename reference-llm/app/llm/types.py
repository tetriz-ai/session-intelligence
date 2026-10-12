# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Shared types for the LLM module."""

from __future__ import annotations

from enum import StrEnum
from typing import Any

from pydantic import BaseModel, Field, model_validator


class ThinkingLevel(StrEnum):
    """Gemini thinking effort — the control for Gemini 3.x models, which replaced the
    raw thinking_budget integer. Values mirror the google-genai ThinkingLevel enum so
    they pass straight to the SDK, but this is our own type so the wire model stays
    SDK-free. See Google's Gemini 3.5 Flash developer guide."""

    MINIMAL = "MINIMAL"
    LOW = "LOW"
    MEDIUM = "MEDIUM"
    HIGH = "HIGH"


class TokenUsage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    # Gemini thinking-model reasoning tokens. Billed at the OUTPUT rate but NOT
    # part of completion_tokens (which is candidates/visible only), so without
    # this field thinking spend is invisible in our token metrics. 0 for
    # a provider that does not report it.
    thoughts_tokens: int = 0
    # System-prompt tokens served from a Gemini explicit cache, read from
    # usage_metadata.cached_content_token_count. These are a SUBSET of prompt_tokens (Gemini
    # counts cached input inside prompt_token_count), NOT an addition — pricing.py bills them
    # at the cached input rate and the remaining prompt_tokens at the full input rate. 0 when
    # caching was not engaged or nothing hit. Surfaced on the response usage as
    # cache_read_tokens (the additive /generate-content response field,).
    cache_read_tokens: int = 0


class ToolChoice(StrEnum):
    """Whether the model may answer in prose, or must call one of the declared tools.

    AUTO lets the model choose between a tool call and a final message. ANY forces a
    tool call, which is what a turn whose every ending is itself a tool call needs.
    NONE declares the tools without permitting a call — the model may only answer.
    Values mirror Gemini's FunctionCallingConfigMode so they pass straight to the SDK,
    but this is our own type so the wire model stays SDK-free.
    """

    AUTO = "AUTO"
    ANY = "ANY"
    NONE = "NONE"


class ToolDeclaration(BaseModel):
    """One tool the model may call: its name, what it is for, and its parameter schema.

    parameters_json_schema is plain JSON Schema, the same shape response_json_schema
    already carries, so a caller that can describe a structured response can describe a
    tool without learning a second vocabulary. It is optional because a tool may take no
    parameters at all.
    """

    name: str
    description: str = ""
    parameters_json_schema: dict[str, Any] | None = None


class ToolCall(BaseModel):
    """The model asking for one tool to run, with the arguments it chose.

    `id` is the vendor's own correlation id when it supplies one; it is echoed back
    unchanged rather than interpreted, and is None for a vendor that does not.

    `thought_signature` is the opaque, base64-encoded token Gemini 3.x attaches to a
    function call and REQUIRES back on the same call when that turn is replayed as
    history. Losing it costs the model its reasoning across the round trip, so it rides
    the wire with the call and is re-attached verbatim when the conversation is rebuilt.
    Base64 because this is a JSON wire contract and the vendor value is raw bytes.
    """

    name: str
    args: dict[str, Any] = Field(default_factory=dict)
    id: str | None = None
    thought_signature: str | None = None


class ToolResult(BaseModel):
    """What running one tool produced, on its way back to the model.

    `response` is a JSON object rather than a string because that is the shape the
    vendor's function-response part takes. A tool that failed still returns a result —
    the caller puts the failure in `response` — since dropping it degrades the model's
    willingness to make parallel calls on later turns.
    """

    name: str
    response: dict[str, Any] = Field(default_factory=dict)
    id: str | None = None


class ConversationRole(StrEnum):
    """Who authored one message in the conversation.

    USER is the person (or the caller speaking for them), MODEL is the assistant's own
    previous turn, and TOOL carries the results of the calls MODEL asked for. TOOL is
    our own role rather than the vendor's: Gemini carries function responses under its
    `user` role, and the provider does that translation so callers never have to encode
    a tool result as something a person said.
    """

    USER = "user"
    MODEL = "model"
    TOOL = "tool"


class ConversationMessage(BaseModel):
    """One message in the conversation the model is asked to continue.

    The fields a message uses follow from its role: USER and MODEL carry `text`, MODEL
    also carries the `tool_calls` it made, and TOOL carries `tool_results` — ALL of a
    round's results in ONE message, never split across several, because splitting them
    degrades parallel tool calling on later turns.
    """

    role: ConversationRole
    text: str | None = None
    tool_calls: list[ToolCall] = Field(default_factory=list)
    tool_results: list[ToolResult] = Field(default_factory=list)


def duplicate_id_error(conversation: list[ConversationMessage]) -> str | None:
    """The reason a conversation's tool ids are ambiguous, or None when they are not.

    The id is what pairs a result with the call it answers, so a repeated one does not fail —
    it pairs the wrong result with the wrong call and the model reasons over a figure it never
    asked for. Two calls to the same tool with different arguments are the ordinary shape of a
    parallel read, so the name cannot do this job.

    Ambiguity is a property of the calls that are still OPEN, not of the whole transcript. A
    call is open from the message that made it until a result answers it; two open calls
    sharing an id cannot be told apart, while an id reused after its call was answered names
    one call at every moment and is not ambiguous at all. Scoping this to the whole
    conversation instead would reject a conversation the vendor is free to produce — an id
    that counts per response restarts at the same value each round — and rejecting a legal
    exchange is the worse failure, since a caller has no way to comply.

    Ids stay optional: a call or result without one is not tracked, because nothing can be
    mispaired by a value that is absent.
    """
    open_ids: set[str] = set()
    for message in conversation:
        for call in message.tool_calls:
            if call.id is None:
                continue
            if call.id in open_ids:
                return f"two unanswered tool calls carry the id {call.id!r}; an id must name one open call"
            open_ids.add(call.id)
        for result in message.tool_results:
            if result.id is None:
                continue
            # A result must close a call that is open. One that does not is either answering
            # a call twice or naming a call that was never made, and both reach the vendor as
            # a response paired to nothing. Checking membership rather than discarding blind
            # is what makes this direction fail: discarding an absent id is a silent no-op.
            if result.id not in open_ids:
                return (
                    f"tool result carries the id {result.id!r}, which answers no open call; "
                    "an id must name the call it answers"
                )
            open_ids.remove(result.id)
    return None


class CompletionRequest(BaseModel):
    model: str
    # Per-task availability-fallback model. When set (and the global fallback flag is on and the
    # task did not disable fallback), complete_with_gemini_fallback runs `model` as primary and
    # this as fallback, instead of the global GEMINI_PRIMARY_MODEL/GEMINI_FALLBACK_MODEL. None →
    # the global pair applies. Set per task via TaskConfig.fallback_model.
    fallback_model: str | None = None
    system_prompt: str = ""
    # The whole input for a single-shot task. Left empty by a conversation request,
    # whose input is `conversation` instead; the validator below enforces exactly one.
    user_content: str = ""
    # Default 0.7 unchanged; the type allows None so a single task (weekly_digest) can carry
    # "unset" → the provider then OMITS temperature so gemini-3.x uses its tuned default.
    temperature: float | None = 0.7
    max_tokens: int | None = None
    response_json_schema: dict[str, Any] | None = None
    # Forwarded by callers for provider-side log correlation. Not sent to the
    # LLM API; not currently consumed by GeminiProvider but kept on the model
    # so future providers can log/route by task without re-plumbing context.
    task_type: str | None = None
    # Per-request override for the Gemini provider HTTP timeout, in seconds. None →
    # the provider's configured REQUEST_TIMEOUT. Set per task via
    # TaskConfig.request_timeout_seconds.
    timeout_seconds: int | None = None
    # Gemini thinking effort (ThinkingLevel). None → model default (currently MEDIUM).
    # This is the gemini-3.x reasoning control. The provider sends it only when the
    # target model is 3.x; on a 2.x model it is ignored in favor of thinking_budget
    # below. Honored by the Gemini path only. Pydantic validates the
    # value against the enum, so an unknown level is rejected at construction.
    thinking_level: ThinkingLevel | None = None
    # Gemini 2.x thinking budget (integer token count). This is the gemini-2.5-flash
    # reasoning control — the pre-3.x knob that thinking_level replaced. The provider
    # sends it only when the target model is 2.x (a 3.x model uses thinking_level
    # instead); None → the 2.x model's default (dynamic) thinking. Carried alongside
    # thinking_level so a task can drive both the 2.5-first and 3.5-fallback models
    # correctly under GEMINI_MODEL_FALLBACK_ENABLED. Honored by the Gemini path only.
    thinking_budget: int | None = None
    # Opt-in Gemini system-prompt caching. When True AND the call routes to Gemini
    # AND a system_prompt is present, the provider references a shared explicit CachedContent
    # for (task_type, model, system_prompt) instead of sending system_instruction inline;
    # cached tokens come back on usage.cache_read_tokens. Ignored when the call does not
    # route to Gemini or there is no system prompt. Default False keeps every existing caller unchanged.
    use_system_prompt_cache: bool = False
    # Send `temperature` to a gemini-3.x model too. The provider's default for 3.x is to OMIT
    # temperature (Google recommends default sampling for 3.x reasoning); a schema-bound
    # classifier whose stored verdict governs every later read opts in here. Set per task via
    # TaskConfig.pin_temperature_on_3x; ignored when temperature is None.
    pin_temperature_on_3x: bool = False
    # Tools the model may call this turn. None → no tool calling; the request is the
    # single-shot prompt-to-JSON call every existing task makes, unchanged. When set, the
    # provider sends them as NATIVE function declarations rather than describing them in the
    # prompt, because prompted-JSON tool emulation is
    # measurably worse at emitting well-formed calls and would fail the read-selection bar for
    # a plumbing reason rather than a reasoning one.
    tools: list[ToolDeclaration] | None = None
    # Whether the model may answer in prose or must call a tool. Ignored when `tools` is None.
    # Default AUTO: a caller that declares tools without an opinion gets the vendor default.
    tool_choice: ToolChoice = ToolChoice.AUTO
    # The conversation the model is asked to continue. None → `user_content` is the whole
    # input, which is every existing task. When set it REPLACES user_content as the input, so
    # a tool result has somewhere to live that is not a string spliced into a question.
    # Both are carried rather than one, because the five registered tasks send a single string
    # and must keep working untouched.
    conversation: list[ConversationMessage] | None = None

    @model_validator(mode="after")
    def _exactly_one_input_channel(self) -> CompletionRequest:
        """A request carries its input as a string OR as a conversation, never both or neither.

        Both would leave the provider choosing which one the caller meant, and the two say
        different things; neither is a request with no question in it, which reaches the vendor
        as an opaque 400 rather than as the local bug it is.
        """
        has_conversation = bool(self.conversation)
        if has_conversation and self.user_content:
            raise ValueError("set user_content or conversation, not both")
        if not has_conversation and not self.user_content:
            raise ValueError("one of user_content or conversation is required")
        if self.conversation:
            # Guarded here too, not only at the wire, because this is what the provider is
            # handed: a caller that builds a request directly gets the same check the
            # endpoint's own request model applies.
            problem = duplicate_id_error(self.conversation)
            if problem is not None:
                raise ValueError(problem)
        return self


class CompletionResponse(BaseModel):
    content: str
    model: str
    provider: str
    usage: TokenUsage | None = None
    # One-time explicit-cache STORAGE cost (USD) this call incurred by CREATING a system-prompt
    # cache — the commitment to store the cached tokens for the cache TTL, which Gemini
    # bills separately from the per-call token cost. 0.0 unless this call created the cache. The
    # endpoint folds it into the response cost_usd; it is not a wire field of its own, so the Go
    # /generate-content contract is unchanged.
    cache_storage_cost_usd: float = 0.0
    # Whether this call actually referenced an explicit system-prompt CachedContent (a create or a
    # hit). False when caching was off, or when it was requested but resolution degraded to an
    # inline system prompt (cache list/create outage, or no budget left) — the call ran uncached.
    # The endpoint stamps the trace cache_key only when this is True, so a degraded call is
    # not labelled with a cache object it never used. Not a wire field; the Go contract is unchanged.
    system_prompt_cache_referenced: bool = False
    # The tool calls the model asked for this turn, in the order it made them. EMPTY means
    # the model answered instead — that emptiness is the whole distinction between "run these
    # and come back" and "here is the final message", so a caller branches on it rather than
    # inspecting `content`. A turn can carry both: a model may narrate and call at once.
    tool_calls: list[ToolCall] = Field(default_factory=list)
