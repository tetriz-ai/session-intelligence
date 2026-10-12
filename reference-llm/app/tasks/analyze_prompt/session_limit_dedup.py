# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Post-processing layer: deterministic deduplication of session-limit events.

WHY THIS EXISTS
---------------
The LLM prompt instructs the model to count distinct AI responses that signal a
token, context-window, or session limit and return that count as
token_analysis.session_limit_hit_count. This works well for detecting signals,
but the LLM consistently fails to apply temporal deduplication: when a user
retries the same prompt 2–3 minutes after hitting the context wall, the model
counts two separate "events" even though both signals are retries against the
same unresolved limit — inflating the metric.

We verified this empirically: a conversation with two consecutive limit messages
3 minutes apart received count=2 from the LLM regardless of how the prompt
instructions were phrased, including explicit worked examples and IDE-specific
guidance. The LLM is reliable at *detecting* limit phrases; temporal arithmetic
across timestamps is where it fails consistently.

WHY A PYTHON POST-PROCESSING LAYER IS THE RIGHT FIX
-----------------------------------------------------
Temporal clustering is deterministic arithmetic on ISO-8601 timestamps, not
language understanding. Python is strictly more reliable than an LLM at it, and
runs in under 0.2 ms p99 on a 9 KB conversation — negligible against the 10–60 s
LLM call. The dedup logic is also independently unit-testable, whereas testing
LLM temporal reasoning requires expensive live calls with non-deterministic output.

The split of responsibilities is therefore clean:
  LLM     — detect which assistant turns contain a limit signal phrase
  Python  — cluster those signals by timestamp and compute the final count

We keep session_limit_hit_count in the LLM schema so Gemini's enforced JSON mode
always outputs it, then overwrite it here when timestamps are present. When the
conversation has no timestamps the LLM's raw count is used unchanged.

DEDUPLICATION RULE
------------------
  gap < 30 min between consecutive limit signals  →  same event
  gap >= 30 min                                   →  new distinct event

The 30-minute threshold covers the full range of assistant retry patterns:
  - Claude Code / Cursor / Gemini: context window fills permanently; any
    rapid-retry cluster within minutes is always the same wall.
  - GitHub Copilot / Codex: time-based session resets typically take 30–60 min,
    so signals separated by < 30 min are almost certainly the same session window.
"""

from __future__ import annotations

import re
from datetime import datetime
from typing import NamedTuple

from app.llm.schemas import AnalyzePromptResponse

# ---------------------------------------------------------------------------
# Limit-signal phrase list — phrase-based superset used for timestamp extraction.
# The LLM additionally detects structural incompleteness (unclosed code fences,
# mid-word truncation, mid-clause prose), which has no equivalent here. The two
# lists intentionally need not be kept in sync: Python is phrase-only; the LLM
# handles structural detection. Any structural-only signal found by the LLM is
# preserved via the structural_excess floor in deduplicate_session_limit_count.
# ---------------------------------------------------------------------------
_LIMIT_PHRASES: tuple[str, ...] = (
    "context window is full",
    "context window limit",
    "conversation is too long",
    "i've reached my token",
    "i've reached my context",
    "i have reached my context",
    "please start a new conversation",
    "maximum token limit reached",
    "context length exceeded",
    "session limit reached",
    "i can no longer process this",
    "chat history has exceeded",
    "cannot process further requests",
    "too long to continue",
    "exceeded the maximum allowed length",
)

# Matches the optional [ISO-8601 UTC] prefix + role label at the start of a turn.
# Group 1: timestamp string (may be None), Group 2: role, Group 3: first content line.
# No leading \s* — turn headers from ConversationToText always start at column 0.
# Indented lines inside a turn's content are never mistaken for a new header.
# "unspecified" covers ROLE_UNSPECIFIED turns emitted as "unspecified: ...".
_TURN_HEADER_RE = re.compile(r"^(?:\[(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\]\s+)?(user|assistant|unspecified):\s*(.*)")

# Two limit signals whose timestamps differ by less than this → same event.
_SAME_EVENT_GAP_S: float = 30 * 60  # 30 minutes


class _LimitSignal(NamedTuple):
    ts: datetime | None  # UTC, or None when the turn had no timestamp prefix
    content: str


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------


def _parse_limit_signals(conversation: str) -> list[_LimitSignal]:
    """Return one entry per assistant turn that contains a limit-signal phrase.

    Uses a line-by-line state machine so multi-paragraph turn content (e.g. a
    code block followed by explanatory text) is captured correctly — splitting
    on blank lines would break those turns.
    """
    signals: list[_LimitSignal] = []
    current_ts: datetime | None = None
    current_role: str | None = None
    current_lines: list[str] = []

    def _flush() -> None:
        if current_role != "assistant" or not current_lines:
            return
        content = "\n".join(current_lines)
        if any(phrase in content.lower() for phrase in _LIMIT_PHRASES):
            signals.append(_LimitSignal(ts=current_ts, content=content))

    for line in conversation.splitlines():
        m = _TURN_HEADER_RE.match(line)
        if m:
            _flush()
            ts_str, role, first_line = m.group(1), m.group(2), m.group(3)
            current_role = role
            current_ts = None
            if ts_str:
                try:
                    current_ts = datetime.fromisoformat(ts_str.replace("Z", "+00:00"))
                except ValueError:
                    pass
            current_lines = [first_line] if first_line.strip() else []
        elif current_role is not None:
            current_lines.append(line)

    _flush()
    return signals


def _cluster_signals(signals: list[_LimitSignal]) -> int:
    """Count distinct limit events by clustering temporally adjacent signals.

    Two consecutive signals are the same event when both carry timestamps and
    their gap is less than _SAME_EVENT_GAP_S. A missing timestamp on either
    signal always starts a new event — we never silently collapse what we
    cannot measure.

    Clustering is intentionally transitive: signals at t=0, t=25, t=50 minutes
    each fall within 30 minutes of the prior one and form a single event. This
    is correct for context-window limits — the window stays full until the user
    opens a new chat, so all retries in the interim are the same unresolved wall.
    """
    if not signals:
        return 0

    events = 0
    last_ts: datetime | None = None

    for sig in signals:
        if last_ts is None or sig.ts is None:
            events += 1
        else:
            gap_s = abs((sig.ts - last_ts).total_seconds())
            if gap_s >= _SAME_EVENT_GAP_S:
                events += 1
        last_ts = sig.ts

    return events


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def deduplicate_session_limit_count(
    response: AnalyzePromptResponse,
    conversation: str,
) -> AnalyzePromptResponse:
    """Overwrite session_limit_hit_count with a timestamp-deduplicated count.

    Returns the response unmodified when:
    - no limit signals are found in the conversation text, or
    - none of the limit signals carry a timestamp (nothing to cluster on).

    When deduplication runs it mutates response.token_analysis.session_limit_hit_count
    in place and returns the same object.
    """
    signals = _parse_limit_signals(conversation)
    if not signals:
        return response

    if not any(s.ts is not None for s in signals):
        # No timestamps available — trust the LLM's count.
        return response

    llm_count = response.token_analysis.session_limit_hit_count
    clustered_count = _cluster_signals(signals)

    # _parse_limit_signals is phrase-only; the LLM also detects structural
    # incompleteness (unclosed fences, mid-word truncation). If the LLM found
    # more signals than phrase-matching did, the excess represents structural
    # signals we cannot cluster. Preserve them as a floor so we don't undercount.
    structural_excess = max(0, llm_count - len(signals))
    response.token_analysis.session_limit_hit_count = clustered_count + structural_excess
    return response
