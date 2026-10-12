# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic schemas for the analyze_prompt task.

Structured output for the Gemini call that scores a coding-agent session:
quality scores, taxonomy-issue detection, token analysis, and per-prompt
breakdown.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import BaseModel, Field, field_validator

from app.llm.schemas.loops import LoopAnalysis


class CategorySlug(StrEnum):
    AMBIGUOUS_DIRECTIVE = "ambiguous-directive"
    MISSING_STATE_DELTA = "missing-state-delta"
    UNSPECIFIED_OUTPUT = "unspecified-output"
    MISSING_INTERNAL_CONTEXT = "missing-internal-context"
    MISSING_EXTERNAL_ENVIRONMENT = "missing-external-environment"
    EXCESSIVE_NOISE = "excessive-noise"
    MISSING_CONSTRAINTS = "missing-constraints"
    TOKEN_WASTE = "token-waste"
    CONTEXT_CONFLICT = "context-conflict"
    API_KEY_LEAK = "api-key-leak"
    DB_CREDENTIAL_LEAK = "db-credential-leak"
    PII_INJECTION = "pii-injection"


# Security-layer slugs a skill must never claim to own. The security guarantee lives
# downstream: security_flags are built from taxonomy_issues (layer=security),
# independent of owned_checks and suppression, so suppression cannot drop a flag at
# persistence. This set just keeps the descriptive owned_checks annotation honest; the
# real residual is the LLM omitting a security taxonomy_issue under injected framing,
# which the prompt SECURITY FLOOR mitigates (LLM detection is inherently injectable).
_SECURITY_SLUGS = frozenset({CategorySlug.API_KEY_LEAK, CategorySlug.DB_CREDENTIAL_LEAK, CategorySlug.PII_INJECTION})


class Provenance(StrEnum):
    # Only USER spans are scored; the rest are context. A StrEnum (like CategorySlug)
    # so Gemini's structured decoding is constrained to these exact values — a stray
    # "skill-baseline" would otherwise silently flip whether a span is scored.
    USER = "user"
    SKILL_BASELINE = "skill_baseline"
    INJECTION = "injection"


class Source(StrEnum):
    USER = "user"  # user typed the invocation
    AUTO = "auto"  # model/harness invoked it


class InputContract(StrEnum):
    NONE = "none"  # autonomous; the skill is the whole package
    TARGET = "target"  # needs to know which artifact (PR/branch)
    TASK = "task"  # needs the user to say what to do / what goes in the output


# Max stored length of a provenance segment's text. Segments exist to mark WHICH
# spans are scored; the full span (esp. an injected skill body) must not be echoed
# into the analysis JSONB, which is exposed on the session-detail path.
_SEGMENT_TEXT_MAX = 200


class QualityScores(BaseModel):
    overall: int = Field(..., ge=1, le=10)
    clarity: int = Field(..., ge=1, le=10)
    specificity: int = Field(..., ge=1, le=10)
    context_provision: int = Field(..., ge=1, le=10)
    actionability: int = Field(..., ge=1, le=10)
    completeness: int = Field(..., ge=1, le=10)
    efficiency: int = Field(..., ge=1, le=10)


class TaxonomyIssue(BaseModel):
    category_slug: CategorySlug | None
    severity: str
    layer: str
    title: str
    description: str
    suggestion: str
    affected_text: str
    estimated_wasted_tokens: int
    prompts: str = Field(..., description="Comma-separated prompt numbers, e.g. '1, 3, 7'")
    action_item: str = Field(
        "",
        description=("For security-layer issues only: one-line recommended action (e.g. 'Expire the leaked API key')"),
    )


class TokenBreakdown(BaseModel):
    category: CategorySlug
    tokens: int
    description: str


class TokenAnalysis(BaseModel):
    total_tokens: int
    wasted_tokens: int
    wasted_percentage: float
    breakdown: list[TokenBreakdown]
    session_limit_hit_count: int = Field(
        ...,
        ge=0,
        description=(
            "Number of distinct session-limit events. Consecutive AI responses signalling "
            "a limit within 30 minutes of each other are collapsed into one event; signals "
            "separated by 30+ minutes count as separate events. 0 if none."
        ),
    )


class PromptSegment(BaseModel):
    provenance: Provenance = Field(
        ..., description="Only user segments are scored; skill_baseline/injection are context."
    )
    text: str = Field(..., description="A SHORT excerpt of the span for display — NOT the full content.")

    @field_validator("text")
    @classmethod
    def _truncate_text(cls, v: str) -> str:
        # Never persist a full span (esp. an injected skill body) into the analysis
        # JSONB, which is exposed on the session-detail path — excerpt for display only.
        return v if len(v) <= _SEGMENT_TEXT_MAX else v[:_SEGMENT_TEXT_MAX].rstrip() + "…"


class PerPromptAnalysis(BaseModel):
    prompt_number: int
    score: int = Field(..., ge=1, le=10)
    feedback: str
    issues: str = Field(..., description="Comma-separated category slugs or 'none'")
    context_reliance: str
    quality_practices: str
    files_modified: str = Field(..., description="Number or 'N/A'")
    lines_changed: str = Field(..., description="Number or 'N/A'")
    improved_prompt: str = Field(
        ...,
        description="Improved version whenever a taxonomy issue is attributed to this prompt, or 'N/A'",
    )
    discard_probability: float = Field(
        ...,
        ge=0.0,
        le=1.0,
        description=(
            "Probability that the AI response to the preceding prompt was "
            "discarded, inferred from this prompt's content. Always 0.0 for "
            "prompt 1."
        ),
    )
    preceding_pair_tokens: int = Field(
        ...,
        description=(
            "Estimated token count of the preceding prompt + AI response "
            "pair. 0 for prompt 1. Added to wasted_tokens if "
            "discard_probability >= threshold."
        ),
    )
    segments: list[PromptSegment] = Field(
        default_factory=list,
        description=(
            "Span-level provenance; only user segments are scored. Empty = "
            "the whole prompt is treated as user-authored."
        ),
    )


class InvokedSkill(BaseModel):
    name: str = Field(..., description="Skill slug, namespaced for plugin skills like plugin:skill")
    source: Source = Field(..., description="user (typed slash-command) or auto (model/harness invoked)")
    baseline_intent: str = Field(..., description="What the skill is expected to do (X)")
    owned_checks: list[str] = Field(
        default_factory=list,
        description="Taxonomy slugs the skill satisfies; suppressed for the user prompts",
    )
    input_contract: InputContract = Field(..., description="What the skill needs from the user: none | target | task")
    user_added: str = Field(..., description="User contribution on top (Y); N/A if none")
    complete: bool = Field(..., description="Whether X+Y is complete for the intent given the session")
    gap_bridge: str = Field("", description="What the user should add to complete it; empty when complete")

    @field_validator("owned_checks")
    @classmethod
    def _drop_security_owned_checks(cls, v: list[str]) -> list[str]:
        # Keep the (descriptive) owned_checks annotation honest — a skill never owns a
        # security check. NOT the security guarantee itself: flags are built downstream
        # from taxonomy_issues independent of owned_checks (see _SECURITY_SLUGS).
        return [c for c in v if c not in _SECURITY_SLUGS]


class AnalyzePromptQualityResponse(BaseModel):
    """The quality contract alone — the guided-decoding schema for a call given no timeline.

    Why the loop field is absent rather than optional: see ``response_schema_for``.
    """

    quality_scores: QualityScores
    # These lists are intentionally UNBOUNDED in the schema. A giant multi-prompt session can
    # emit one per_prompt_analysis row per prompt and overrun max_output_tokens (65,536, shared
    # with thinking), truncating at MAX_TOKENS — surfaced as HTTP 502. The obvious fix (Pydantic
    # max_length → JSON-schema maxItems) does NOT work here: this task runs on gemini-2.5-flash,
    # whose structured-output serving limit rejects ANY maxItems on these nested-item lists with
    # a 400 "too many states for serving" (verified live; the same limit forced generate_insights
    # onto a 3.x model — see router.py). That 400 is a non-retryable LLMInvalidRequestError, so it
    # would take down EVERY call, not just the giant tail. The row bound is therefore enforced by
    # the prompt (a soft ceiling the model honours), not the schema — see ANALYZE_PROMPT_SYSTEM's
    # LIST LIMITS bullet. Do not add max_length to these fields while the task targets 2.5-flash.
    taxonomy_issues: list[TaxonomyIssue]
    token_analysis: TokenAnalysis
    improved_prompt: str
    recommendations: list[str]
    per_prompt_analysis: list[PerPromptAnalysis]
    task_type: str = Field(
        ...,
        description=("One of: bug-fix, feature, refactor, debug, code-review, documentation, testing, devops, other"),
    )
    ai_reviewed: bool = Field(
        ...,
        description=(
            "true if the user explicitly asked the AI to review code or a PR in this session "
            "(e.g., 'review this code', 'review this PR', '/review', '/code-review', 'audit my changes'). "
            "false if no review was explicitly requested by the user in any prompt."
        ),
    )
    # no_user_prompt is intentionally NOT an LLM output: a skill-only conversation is derived
    # deterministically on the Go side from per-prompt segment provenance (none user-authored),
    # so an omitted/defaulted flag can never persist a skill-only session as an ordinary one.
    invoked_skills: list[InvokedSkill] = Field(
        default_factory=list,
        description="Skills/commands that drove the session; empty for organic prompting.",
    )


class AnalyzePromptResponse(AnalyzePromptQualityResponse):
    """Quality plus loops; also the parsing model for a quality-only response, where
    ``loops`` is simply absent. Narrowing applies to what the model is ASKED for, not to
    what the server accepts.
    """

    # Additive: every existing quality field keeps its top-level position, so
    # existing consumers of the quality response do not migrate. The loop block is
    # validated separately from the quality fields, which is what lets one of the two
    # be malformed without discarding the other.
    loops: LoopAnalysis | None = Field(
        None,
        description=("Loop analysis for this session. Omit entirely when loop analysis was not requested."),
    )
