# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Loop-analysis schemas for the analyze_prompt task.

Enum VALUES are the full proto enum names (`LOOP_RUN_STATUS_SUCCEEDED`, not
`SUCCEEDED`). That is load-bearing, not verbosity: Go parses this block with
`protojson`, which does NOT reject an unrecognized enum string — it silently yields the
zero value. A short name would therefore land as UNSPECIFIED, so a free-form run would
skip the correction-cycle evidence rule that gates it and no run would ever read
SUCCEEDED. The schema is what steers the model, so it has to carry the exact names.

The model returns *locators and typed classifications only* — never display prose.
The engine derives every stored excerpt from the normalized events a locator points
at, so nothing the model writes crosses into the database. That boundary is the
reason these schemas carry no free-text description fields.
"""

from enum import StrEnum

from pydantic import BaseModel, Field


class EventCoverage(StrEnum):
    """How completely the uploaded event timeline represents the source."""

    COMPLETE = "EVENT_COVERAGE_COMPLETE"
    PARTIAL = "EVENT_COVERAGE_PARTIAL"
    NONE = "EVENT_COVERAGE_NONE"


class LoopInitiatorKind(StrEnum):
    """What started a run. FREE_FORM carries the strictest evidence bar."""

    FREE_FORM = "LOOP_INITIATOR_KIND_FREE_FORM"
    COMMAND = "LOOP_INITIATOR_KIND_COMMAND"
    SKILL = "LOOP_INITIATOR_KIND_SKILL"
    HOOK = "LOOP_INITIATOR_KIND_HOOK"
    OTHER = "LOOP_INITIATOR_KIND_OTHER"


class LoopRunStatus(StrEnum):
    """A run's terminal state. INCOMPLETE and ABORTED are distinct from FAILED:
    unfinished work must never be reported as terminal failure."""

    SUCCEEDED = "LOOP_RUN_STATUS_SUCCEEDED"
    FAILED = "LOOP_RUN_STATUS_FAILED"
    INCOMPLETE = "LOOP_RUN_STATUS_INCOMPLETE"
    ABORTED = "LOOP_RUN_STATUS_ABORTED"


class LoopIterationActionKind(StrEnum):
    INITIAL = "LOOP_ITERATION_ACTION_KIND_INITIAL"
    CORRECTION = "LOOP_ITERATION_ACTION_KIND_CORRECTION"


class LoopCheckKind(StrEnum):
    TEST = "LOOP_CHECK_KIND_TEST"
    BUILD = "LOOP_CHECK_KIND_BUILD"
    LINT = "LOOP_CHECK_KIND_LINT"
    REVIEW = "LOOP_CHECK_KIND_REVIEW"
    BROWSER = "LOOP_CHECK_KIND_BROWSER"
    COMMAND_RESULT = "LOOP_CHECK_KIND_COMMAND_RESULT"
    AGENT_JUDGMENT = "LOOP_CHECK_KIND_AGENT_JUDGMENT"
    OTHER = "LOOP_CHECK_KIND_OTHER"


class LoopCheckOutcome(StrEnum):
    PASS = "LOOP_CHECK_OUTCOME_PASS"
    FAIL = "LOOP_CHECK_OUTCOME_FAIL"
    PARTIAL = "LOOP_CHECK_OUTCOME_PARTIAL"
    UNKNOWN = "LOOP_CHECK_OUTCOME_UNKNOWN"


class LoopCheckIndependence(StrEnum):
    """Whether the check's VERDICT was authored by something other than the actor.

    Authorship, not invocation: an agent that runs the test suite itself still gets an
    INDEPENDENT verdict, because it cannot argue with a failing test. A quality
    dimension only — an agent checking its own work is still a valid loop.
    """

    SELF = "LOOP_CHECK_INDEPENDENCE_SELF"
    INDEPENDENT = "LOOP_CHECK_INDEPENDENCE_INDEPENDENT"
    UNKNOWN = "LOOP_CHECK_INDEPENDENCE_UNKNOWN"


class LoopWorkType(StrEnum):
    """The kind of engineering work a run shipped.

    It classifies the WORK, never the loop's quality: a run that fixed a bug is BUG_FIX
    whether or not it succeeded. UNSPECIFIED is absent deliberately — the model must
    choose, and OTHER is the escape hatch for recognisable work outside these classes.
    A run Go stores as UNSPECIFIED is one no analyzer classified, which is a different
    statement from "no class fits".
    """

    FEATURE = "LOOP_WORK_TYPE_FEATURE"
    BUG_FIX = "LOOP_WORK_TYPE_BUG_FIX"
    REFACTOR = "LOOP_WORK_TYPE_REFACTOR"
    TESTS = "LOOP_WORK_TYPE_TESTS"
    CHORE = "LOOP_WORK_TYPE_CHORE"
    INFRA = "LOOP_WORK_TYPE_INFRA"
    OTHER = "LOOP_WORK_TYPE_OTHER"


class EventLocator(BaseModel):
    # ge=1, never gt=0: the provider's response-schema validator rejects the
    # exclusiveMinimum keyword that gt renders, which would fail every loop
    # inference before it reached the model. Identical bound for an integer.
    sequence: int = Field(
        ...,
        ge=1,
        description=(
            "The one-based sequence of a session event supplied in session_events. "
            "Must reference an event that was actually provided — an invented "
            "sequence invalidates the whole loop block."
        ),
    )


class LoopCheckAnalysis(BaseModel):
    kind: LoopCheckKind
    name: str = Field("", max_length=128, description="Checker name, e.g. the test target or linter.")
    outcome: LoopCheckOutcome
    independence: LoopCheckIndependence
    locators: list[EventLocator] = Field(
        default_factory=list,
        description="Events proving this check ran. Must be at or after the iteration's action.",
    )


class LoopIterationAnalysis(BaseModel):
    ordinal: int = Field(..., ge=1, description="One-based and contiguous within the run; 1 is the initial action.")
    action_kind: LoopIterationActionKind
    action_locators: list[EventLocator] = Field(default_factory=list)
    checks: list[LoopCheckAnalysis] = Field(default_factory=list)


class LoopRunAnalysis(BaseModel):
    run_ordinal: int = Field(..., ge=1, description="One-based and contiguous within the session.")
    initiator_kind: LoopInitiatorKind
    initiator_name: str = Field(
        "",
        max_length=128,
        description="Declared command/skill/hook name; empty for FREE_FORM.",
    )
    status: LoopRunStatus
    objective_locator: EventLocator | None = Field(
        None,
        description="The event that best states what the run was trying to achieve.",
    )
    parent_run_ordinal: int = Field(
        0,
        ge=0,
        description="Nests this run inside another run of the same session; 0 means top-level.",
    )
    work_type: LoopWorkType = Field(
        ...,
        description=(
            "The kind of work this run shipped, judged from what changed — not from "
            "whether the run succeeded. Use OTHER only when the work is recognisable "
            "but fits none of the classes."
        ),
    )
    iterations: list[LoopIterationAnalysis] = Field(default_factory=list)


class LoopAnalysis(BaseModel):
    event_coverage: EventCoverage
    runs: list[LoopRunAnalysis] = Field(default_factory=list)


class AnalyzeLoopsResponse(BaseModel):
    """The loops_only contract used for child (subagent / plugin / hook) sessions.

    Prompt quality is deliberately absent, not optional: children are never scored
    for prompt quality, so a schema that merely made it optional would invite the
    model to emit it anyway.
    """

    loops: LoopAnalysis
