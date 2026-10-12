# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Analysis-mode selection and sibling-block extraction for analyze_prompt.

Two contracts share one inference:

* ``quality_and_loops`` — a primary (human-driven) session: prompt quality plus loops.
* ``loops_only`` — a child (subagent / plugin / hook) session: loops only, with
  prompt-quality scoring prohibited.

An absent mode means the caller predates loop analysis, so the flat quality-only
contract is used unchanged. That is what makes a rolling deploy safe in both
directions: new Python serves an old Go caller, and new Go tolerates old Python
answering without a loop block.
"""

from __future__ import annotations

import json
from typing import Any

from pydantic import TypeAdapter, ValidationError

from app.llm.schemas import (
    AnalyzeLoopsResponse,
    AnalyzePromptQualityResponse,
    AnalyzePromptResponse,
    LoopAnalysis,
)

# LOOP_CONTRACT_VERSION names the loop contract this service answers under — the prompt in
# `app.llm.prompts.loops` plus the admission rules it states. It MUST equal
# `loopanalysis.AnalyzerVersion` on the Go side, which refuses to persist any snapshot whose
# response does not positively claim the contract that build speaks. The two are a pair:
# a change to what the contract admits bumps both in one change, and a bump on either side
# alone stops loop snapshots being stored until the other catches up (deliberately — the
# alternative is storing rows produced under rules this build already rejected).
#
# v6 carries no prompt change. It moves because the Go side's derivation of a loop's TYPE from
# the model's answer changed — the pair is one version, so the stamp advances with it or every
# snapshot is rejected as skew. Read the rationale beside `AnalyzerVersion`; do not infer from
# this bump that the prompt was edited.
#
# v7 is the same shape: no prompt change, a Go-side derivation change. A check whose `name` the
# model leaves empty is now named from the event it cites. The model is still asked for a name and
# its answer still wins; only the empty case moved.
#
# v8 IS a prompt change: a free-form run now requires an agent REVIEW to have driven its correction
# cycle — at least one REVIEW or AGENT_JUDGMENT check. A cycle gated only by a test, build, lint,
# browser check, or command exit is first-pass verification, not a self-reviewing loop. A v7
# free-form snapshot can carry a gate-only run this contract no longer admits, so the pair advances.
#
# v9 IS a prompt change with no change to the Go derivation: it scopes v8's correction-cycle +
# agent-review requirement to FREE-FORM runs, matching what the Go validator already enforces (the
# cycle bar runs only for free-form runs; /loop and /goal are exempt from the autonomy bar). A
# declared run is now reported where the timeline shows it executing without a correction cycle —
# the built-in /loop and /goal on execution alone, every other declared run with its action plus
# one check — so the model stops omitting the legitimate single-iteration declared runs v8's
# over-strict prose made it drop. The pair advances with `loopanalysis.AnalyzerVersion`; the bump
# invalidates the key but does not re-analyse processed records (see the note there).
LOOP_CONTRACT_VERSION = "v9"

# The envelope field the version is stamped on. Read by Go's `extractLoopContractVersion`.
LOOP_CONTRACT_VERSION_KEY = "loop_contract_version"

# Mode values as the Go caller sends them (proto enum names).
MODE_UNSPECIFIED = "ANALYSIS_MODE_UNSPECIFIED"
MODE_QUALITY_AND_LOOPS = "ANALYSIS_MODE_QUALITY_AND_LOOPS"
MODE_LOOPS_ONLY = "ANALYSIS_MODE_LOOPS_ONLY"

SESSION_CTX_ANALYSIS_MODE = "analysis_mode"
SESSION_CTX_LOOP_CANDIDATES = "loop_candidates"
SESSION_CTX_SESSION_EVENTS = "session_events"
# Present and true only when the org reports DECLARED loops only. Absent means both
# kinds, which is what every caller predating the flag sends.
SESSION_CTX_DECLARED_ONLY = "declared_only"
# What the sessions THIS one dispatched verified, as counts. Present only when a child was
# actually read; absent means none was, which is not the same as none existing.
SESSION_CTX_CHILD_ROLLUP = "child_rollup"
# Present and true only when the session was driven by an agent rather than a person.
# Absent means human-driven, which is what every caller predating the key sends.
SESSION_CTX_DELEGATED_SESSION = "delegated_session"

_LOOP_ADAPTER: TypeAdapter[LoopAnalysis] = TypeAdapter(LoopAnalysis)


def requested_mode(session_context: dict[str, Any] | None) -> str:
    """Return the requested analysis mode, defaulting to the quality-only contract."""
    if not session_context:
        return MODE_UNSPECIFIED
    mode = session_context.get(SESSION_CTX_ANALYSIS_MODE)
    if mode in (MODE_QUALITY_AND_LOOPS, MODE_LOOPS_ONLY):
        return str(mode)
    return MODE_UNSPECIFIED


def is_delegated_session(session_context: dict[str, Any] | None) -> bool:
    """Report whether the CALLER stated this session was agent-driven.

    Read from an explicit key rather than derived from the analysis mode. A loops-only
    request means only that the call answers the loop contract alone, which is equally
    true of a human-driven primary session — so treating that mode as delegation would
    waive the autonomy bar for sessions with a real person in them.
    """
    if not session_context:
        return False
    return session_context.get(SESSION_CTX_DELEGATED_SESSION) is True


def wants_loops(mode: str) -> bool:
    return mode in (MODE_QUALITY_AND_LOOPS, MODE_LOOPS_ONLY)


def wants_quality(mode: str) -> bool:
    """Child sessions are never scored for prompt quality."""
    return mode != MODE_LOOPS_ONLY


def response_schema_for(mode: str) -> dict[str, Any]:
    """Return the generation schema for a mode.

    Every mode gets a schema carrying exactly the blocks it asked for and no others, so
    guided decoding cannot produce a block the call has no grounds to answer: a loops-only
    call cannot emit prompt quality, and a quality-only call cannot emit a loop block it
    was given no timeline for. Only the combined mode advertises both.
    """
    if mode == MODE_LOOPS_ONLY:
        return AnalyzeLoopsResponse.model_json_schema()
    if mode == MODE_QUALITY_AND_LOOPS:
        return AnalyzePromptResponse.model_json_schema()
    return AnalyzePromptQualityResponse.model_json_schema()


class BlockValidation:
    """Per-block validation outcome.

    ``valid`` and ``error`` are reported per block rather than for the response as a
    whole: a structurally broken loop block must not prevent a good quality block
    from being persisted, and vice versa.
    """

    __slots__ = ("valid", "error")

    def __init__(self, valid: bool, error: str = "") -> None:
        self.valid = valid
        self.error = error

    def as_dict(self) -> dict[str, Any]:
        return {"valid": self.valid, "error": self.error}


def validate_blocks(result_json: str, mode: str) -> tuple[BlockValidation, BlockValidation]:
    """Validate the quality and loop blocks independently.

    The envelope is parsed minimally first. Invalid top-level JSON, a non-object
    response, or duplicate top-level keys invalidate EVERY block required by the mode
    — at that point nothing about the response can be trusted. Once the envelope
    parses, each block is validated on its own.
    """
    quality_required = wants_quality(mode)
    loops_required = wants_loops(mode)

    try:
        envelope = _parse_object_no_duplicates(result_json)
    except ValueError as exc:  # JSONDecodeError and the duplicate-key guard both land here
        failure = BlockValidation(False, f"envelope: {exc}")
        return (
            failure if quality_required else BlockValidation(True),
            failure if loops_required else BlockValidation(True),
        )

    quality = BlockValidation(True)
    if quality_required:
        # The quality fields are flat at the top level, so the envelope minus `loops`
        # is what the quality contract validates against.
        candidate = {k: v for k, v in envelope.items() if k != "loops"}
        try:
            AnalyzePromptResponse.model_validate(candidate)
        except ValidationError as exc:
            quality = BlockValidation(False, _summarize(exc))

    loops = BlockValidation(True)
    if loops_required:
        raw = envelope.get("loops")
        if raw is None:
            loops = BlockValidation(False, "loops block absent")
        else:
            try:
                _LOOP_ADAPTER.validate_python(raw)
            except ValidationError as exc:
                loops = BlockValidation(False, _summarize(exc))

    return quality, loops


def stamp_loop_contract_version(result_json: str) -> str:
    """Return the response envelope with this service's loop-contract version stamped on it.

    The stamp is written HERE, by the server, and overwrites whatever occupies the key —
    a value the model authored would let a hallucinated string vouch for the block, which
    is the one thing the receiving gate exists to prevent.

    An envelope that does not parse, or is not a JSON object, is returned untouched: there
    is nothing to stamp, and the reader treats a missing stamp exactly as it treats a
    wrong one.
    """
    try:
        envelope = json.loads(result_json)
    except ValueError:
        return result_json
    if not isinstance(envelope, dict):
        return result_json
    envelope[LOOP_CONTRACT_VERSION_KEY] = LOOP_CONTRACT_VERSION
    return json.dumps(envelope)


def _parse_object_no_duplicates(result_json: str) -> dict[str, Any]:
    """Parse a JSON object, rejecting duplicate keys.

    ``json.loads`` silently keeps the last value for a duplicated key, which would let
    a response carry two different ``loops`` blocks and have one win invisibly.
    Rejecting the ambiguity is the only honest reading, so the hook raises rather than
    picking a winner. Duplicates are rejected at every depth, not just the top level:
    a duplicated key inside a run would be the same silent-overwrite problem one level
    down.
    """

    def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        out: dict[str, Any] = {}
        for key, value in pairs:
            if key in out:
                raise ValueError(f"duplicate key {key!r}")
            out[key] = value
        return out

    parsed = json.loads(result_json, object_pairs_hook=_no_duplicates)
    if not isinstance(parsed, dict):
        raise ValueError("response is not a JSON object")
    return parsed


def _summarize(exc: ValidationError, limit: int = 3) -> str:
    """Bound the error text: it travels back to Go and is persisted as a diagnostic."""
    parts = []
    for err in exc.errors()[:limit]:
        loc = ".".join(str(p) for p in err.get("loc", ()))
        parts.append(f"{loc}: {err.get('msg', 'invalid')}")
    return "; ".join(parts)
