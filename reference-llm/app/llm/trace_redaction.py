# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Trace redaction at the Langfuse export boundary.

The `@observe` decorator and `traced_complete` capture the full request / system_prompt /
user_content as a generation's trace input. For the three session-analysis producers that
input carries the conversation transcript, tenant skill bodies, and the per-tenant catalogue
— content whose trace-retention question is entirely different from token counts. Declaring
a "data channel" does not change what actually gets exported, so for these tasks the trace
input is passed through this hook BEFORE the span exports: the user_content (which carries
all of that customer-authored text) is withheld and replaced by a length-only marker, so a
planted skill-body / excerpt marker can never reach an exported trace input. The authored
system prompt is static analyser instructions and stays, so a trace is still legible.

This is the export-boundary control. It is distinct from the prompt-injection guard the
PRD also requires on `analyze_evidence` (which lifts mandatory-looking clauses out of skill
bodies in the PROMPT so the model treats them as data): that guard shapes what the model
reads; this hook shapes what Langfuse stores.

Both DIRECTIONS of that shape live here, and only one of them redacts. `trace_output` is the
single place a response becomes what a span stores, and it withholds nothing today — it is
here so that a scrubber, when there is one worth applying, is added in one function rather
than at every call site.
"""

from __future__ import annotations

from app.llm.router import SESSION_ANALYSIS_TASK_TYPES
from app.llm.types import CompletionRequest, CompletionResponse

# What replaces withheld user content on the trace. Content-free and length-only: a reader
# learns the payload size, never a byte of the customer text.
_WITHHELD = "[redacted: {n} chars of session content withheld from trace export]"


def withheld_content_marker(n: int) -> str:
    """The length-only marker that replaces withheld session content on a trace.

    The single source for this string so the generation-span and the outer @observe-span
    redactions cannot drift apart (both are export-boundary controls for).
    """
    return _WITHHELD.format(n=n)


def trace_input_requires_redaction(task_type: str | None) -> bool:
    """Whether this task's user_content must be withheld from an exported trace.

    The session-analysis producers are ALWAYS redacted (customer transcript + tenant skill
    bodies). For every other task the default is also to redact — no raw transcript leaves to
    tracing — unless an operator explicitly opts into raw export via
    langfuse.SEND_RAW_TRANSCRIPTS.
    """
    if task_type in SESSION_ANALYSIS_TASK_TYPES:
        return True
    # Imported lazily to avoid a config<->llm import cycle at module load.
    from app.config.config import get_config

    return not get_config().langfuse.SEND_RAW_TRANSCRIPTS


def conversation_tool_calls(response: CompletionResponse) -> list[dict[str, object]]:
    """A turn's tool calls, as a trace records them.

    The arguments are kept, not just the names. Which tool ran is half the fact; which tab or
    window it ran against is the other half, and it is the half that answers what the surface
    is actually used for. The thought signature is dropped instead — it is an opaque blob the
    vendor round-trips and no reader can act on, so it is bulk rather than signal.
    """
    return [call.model_dump(exclude={"thought_signature"}) for call in response.tool_calls]


def trace_output(request: CompletionRequest, response: CompletionResponse) -> object:
    """What a generation span exports.

    A single-shot task exports its content, which is what the trace has always carried. A
    conversation exports its tool calls beside that content, because a turn that calls a tool
    and says nothing otherwise has an empty content field — recording only the prose would
    file the turn as though it did nothing at all.

    Nothing is withheld here. That is deliberate, and it is the reason this function exists
    rather than the caller inlining it: the export shape is decided in ONE place, so scrubbing
    it later is one edit covering every task rather than fourteen.
    """
    if request.conversation is None:
        return response.content
    return {"content": response.content, "tool_calls": conversation_tool_calls(response)}


def redacted_trace_input(request: CompletionRequest) -> dict[str, str]:
    """The safe trace-input dict for a generation.

    For a session-analysis task the user_content is withheld (replaced by a length marker);
    every other task keeps the raw input it captures today, so this narrows nothing for the
    existing tasks. The system prompt is always kept — it is authored analyser text, not
    customer content.
    """
    if request.conversation is not None:
        # A conversation's input is its messages, not `user_content`, which a conversation
        # request leaves empty — so reporting that field would export a turn as though it had
        # no input at all.
        #
        # Shape, not text, and the asymmetry with the output is deliberate rather than an
        # oversight. An input grows without bound: it carries the customer's raw words and
        # every whole result set the tools returned, re-sent in full on each later round. The
        # output is the answer drawn from them. Storing the answer is worth its size; storing
        # every row it was drawn from is not, on a surface nobody reads it back from.
        # The size counts the tool results too, not just the message text. They are the bulk
        # of a late-round conversation, so a marker that measured only the prose would report
        # a number far smaller than what it actually stood in for.
        withheld_chars = sum(
            len(message.text or "") + sum(len(str(result.response)) for result in message.tool_results)
            for message in request.conversation
        )
        return {
            "system_prompt": request.system_prompt,
            "conversation": withheld_content_marker(withheld_chars) + f" across {len(request.conversation)} messages",
        }
    if trace_input_requires_redaction(request.task_type):
        return {
            "system_prompt": request.system_prompt,
            "user_content": withheld_content_marker(len(request.user_content)),
        }
    return {"system_prompt": request.system_prompt, "user_content": request.user_content}
