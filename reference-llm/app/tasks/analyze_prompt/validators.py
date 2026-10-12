# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Runtime output validators for the analyze_prompt task.

All validators run after the LLM returns its JSON response and before the result
is returned to the caller. Validation is a side-effect only:
a validator crash or BLOCKING failure never prevents the response from being returned.

Severity levels:
  BLOCKING  — structural/arithmetic error; makes data untrustworthy.
              Sets ValidationResult.has_blocking=True and ValidationResult.passed=False.
              Triggers an error-level log in the handler.
  ADVISORY  — quality signal worth tracking; not definitively wrong.
              Does not affect ValidationResult.passed.
              Logged to Langfuse as a score of 0 for the evaluator.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from app.llm.schemas import AnalyzePromptResponse, CategorySlug

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# Valid task_type values — derived from the system prompt and schema definition.
# Do NOT use the contract approximation — these are the actual values.
VALID_TASK_TYPES: frozenset[str] = frozenset(
    {
        "bug-fix",
        "feature",
        "refactor",
        "debug",
        "code-review",
        "documentation",
        "testing",
        "devops",
        "other",
    }
)

# Valid category slugs — derived from the CategorySlug enum in schemas.py.
VALID_CATEGORY_SLUGS: frozenset[str] = frozenset(slug.value for slug in CategorySlug)

# Security-layer slugs. Their presence forces the 3/10 HARD CAP regardless of the
# severity label the LLM assigned (Layer-4 rates them at least `high`, rarely `critical`),
# matching the SCORING CALIBRATION rule in analyze_prompt.py. Mirrors _SECURITY_SLUGS in
# schemas.py; kept local so this validator does not lean on a private schema symbol.
_SECURITY_SLUGS: frozenset[CategorySlug] = frozenset(
    {
        CategorySlug.API_KEY_LEAK,
        CategorySlug.DB_CREDENTIAL_LEAK,
        CategorySlug.PII_INJECTION,
    }
)

# The 3/10 ceiling the SCORING CALIBRATION HARD CAP imposes for a security/critical issue.
_HARD_CAP_SCORE = 3

# Langfuse evaluator IDs — must match what is registered in Langfuse config.yaml exactly.
EVALUATOR_PER_PROMPT = "ap_per_prompt_consistency_code"
EVALUATOR_CALIBRATION = "ap_score_calibration_code"
EVALUATOR_ISSUE_DETECTION = "ap_issue_detection_code"
EVALUATOR_TASK_CLASS = "ap_task_classification_code"
EVALUATOR_RECOMMENDATION = "ap_recommendation_quality_code"

# ---------------------------------------------------------------------------
# Result dataclasses
# ---------------------------------------------------------------------------


@dataclass
class ValidationError:
    code: str
    evaluator_id: str
    severity: Literal["blocking", "advisory"]
    detail: str
    expected: str  # always stringified — proto field is string
    actual: str  # always stringified


@dataclass
class ValidationResult:
    passed: bool  # False only if any BLOCKING error is present
    errors: list[ValidationError]
    scores: dict[str, int]  # evaluator_id → 0 (fail) or 1 (pass)
    has_blocking: bool
    has_advisory: bool


# ---------------------------------------------------------------------------
# Normalisation helpers
# ---------------------------------------------------------------------------


def normalize_issues(issues: str) -> list[str]:
    """Normalise the per_prompt_analysis.issues field.

    Handles:
      - "none" or "" → []
      - "ambiguous-directive, token-waste" → ["ambiguous-directive", "token-waste"]
      - "ambiguous-directive" → ["ambiguous-directive"]
    """
    if not issues or issues.strip().lower() == "none":
        return []
    return [s.strip() for s in issues.split(",") if s.strip()]


def normalize_prompt_refs(prompts: str) -> list[int]:
    """Normalise the taxonomy_issues.prompts field to a list of ints.

    Handles comma-separated strings like "1, 3, 7" → [1, 3, 7].
    Non-digit parts are silently skipped.
    """
    return [int(p) for p in (p.strip() for p in str(prompts).split(",")) if p.isdigit()]


# ---------------------------------------------------------------------------
# Individual validators
# ---------------------------------------------------------------------------


def _check_ppa2(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """PPA-2: every issue slug in per_prompt_analysis must exist in taxonomy_issues."""
    taxonomy_slugs = {issue.category_slug.value for issue in output.taxonomy_issues if issue.category_slug is not None}

    for prompt in output.per_prompt_analysis:
        slugs = normalize_issues(prompt.issues)
        for slug in slugs:
            if slug not in taxonomy_slugs:
                errors.append(
                    ValidationError(
                        code="PPA_2_SLUG_NOT_IN_TAXONOMY",
                        evaluator_id=EVALUATOR_PER_PROMPT,
                        severity="blocking",
                        detail=(
                            f"prompt {prompt.prompt_number}: slug '{slug}' not found in taxonomy_issues category_slugs"
                        ),
                        expected=f"one of {sorted(taxonomy_slugs)}",
                        actual=slug,
                    )
                )


def _check_ppa3(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """PPA-3: every prompt referenced in taxonomy_issues must appear in per_prompt_analysis
    with non-"none" issues (reverse of PPA-2)."""
    per_prompt_map = {p.prompt_number: p for p in output.per_prompt_analysis}

    for issue in output.taxonomy_issues:
        for prompt_num in normalize_prompt_refs(issue.prompts):
            if prompt_num not in per_prompt_map:
                errors.append(
                    ValidationError(
                        code="PPA_3_TAXONOMY_PROMPT_NOT_FLAGGED",
                        evaluator_id=EVALUATOR_PER_PROMPT,
                        severity="blocking",
                        detail=(
                            f"taxonomy issue '{issue.category_slug}' references prompt {prompt_num} "
                            f"but that prompt is not in per_prompt_analysis"
                        ),
                        expected=f"prompt {prompt_num} in per_prompt_analysis",
                        actual="not found",
                    )
                )
                continue

            ppa = per_prompt_map[prompt_num]
            if not normalize_issues(ppa.issues):
                errors.append(
                    ValidationError(
                        code="PPA_3_TAXONOMY_PROMPT_NOT_FLAGGED",
                        evaluator_id=EVALUATOR_PER_PROMPT,
                        severity="blocking",
                        detail=(
                            f"taxonomy issue '{issue.category_slug}' references prompt {prompt_num} "
                            f"but per_prompt_analysis[{prompt_num}].issues is 'none'"
                        ),
                        expected=f"non-empty issues for prompt {prompt_num}",
                        actual="none",
                    )
                )


def _check_ppa1(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """PPA-1 (ADVISORY): mean of per-prompt scores should be within tolerance of overall score.
    Tolerance is 0.0 for single-prompt sessions, 1.5 for multi-prompt sessions."""
    ppa = output.per_prompt_analysis
    if not ppa:
        return

    # A >80-prompt session emits only a SELECTED sample of rows (see the prompt's SELECTING THE 80
    # ROWS), so the emitted prompt_numbers are sparse and their max exceeds the row count. That
    # sample is deliberately low-biased (lowest-scoring prompts are prioritized), so its mean is not
    # meant to track the whole-session overall — skip PPA-1 for it, else it false-fires on nearly
    # every large session. A COMPLETE session numbers its rows 1..N contiguously (max == count) and
    # is still validated, including one with exactly 80 prompts; a response that ignores the cap and
    # returns one row per prompt also has max == count and is validated, so a real calibration
    # regression is never hidden. (prompt_number is a distinct position, so max >= count always;
    # max > count means, and only means, rows were dropped.)
    if max(p.prompt_number for p in ppa) > len(ppa):
        return

    overall = output.quality_scores.overall
    mean_score = sum(p.score for p in ppa) / len(ppa)
    tolerance = 0.0 if len(ppa) == 1 else 1.5

    if abs(mean_score - overall) > tolerance:
        errors.append(
            ValidationError(
                code="PPA_1_SCORE_MEAN_DIVERGENCE",
                evaluator_id=EVALUATOR_PER_PROMPT,
                severity="advisory",
                detail=(
                    f"mean per-prompt score ({mean_score:.2f}) diverges from "
                    f"overall ({overall}) by {abs(mean_score - overall):.2f} "
                    f"(tolerance: {tolerance})"
                ),
                expected=f"within {tolerance} of {overall}",
                actual=f"{mean_score:.2f}",
            )
        )


def _check_qs1(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """QS-1 (ADVISORY): overall score must be within ±1.5 of the mean of the 6 dimensions.

    The ±1.5 band is mirrored in the SCORING CALIBRATION COHERENCE rule in analyze_prompt.py
    ("overall must stay within ~1.5 of the dimension mean"). Keep the two in sync — tuning one
    without the other makes the prompt instruct a band this advisory no longer measures.
    """
    qs = output.quality_scores
    dimension_keys = ["clarity", "specificity", "context_provision", "actionability", "completeness", "efficiency"]
    dim_mean = sum(getattr(qs, k) for k in dimension_keys) / len(dimension_keys)

    if abs(qs.overall - dim_mean) > 1.5:
        errors.append(
            ValidationError(
                code="QS_1_OVERALL_DIM_DIVERGENCE",
                evaluator_id=EVALUATOR_CALIBRATION,
                severity="advisory",
                detail=(
                    f"overall score ({qs.overall}) diverges from dimension mean "
                    f"({dim_mean:.2f}) by {abs(qs.overall - dim_mean):.2f}"
                ),
                expected=f"within 1.5 of {dim_mean:.2f}",
                actual=str(qs.overall),
            )
        )


def _check_qs2(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """QS-2 (ADVISORY): if any per-prompt score ≤4, overall must be < 8.5."""
    ppa = output.per_prompt_analysis
    if not ppa:
        return

    min_score = min(p.score for p in ppa)
    overall = output.quality_scores.overall
    if min_score <= 4 and overall >= 8.5:
        errors.append(
            ValidationError(
                code="QS_2_INFLATION_SUSPECTED",
                evaluator_id=EVALUATOR_CALIBRATION,
                severity="advisory",
                detail=(
                    f"overall score ({overall}) is >= 8.5 despite a per-prompt score of {min_score} — "
                    f"possible score inflation"
                ),
                expected="overall < 8.5",
                actual=str(overall),
            )
        )


def _check_qs3(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """QS-3 (ADVISORY): the security/critical HARD CAP must hold. A security-layer issue
    (any severity) or a `critical`-severity issue caps overall AND every attributed per-prompt
    score at 3/10 (SCORING CALIBRATION in analyze_prompt.py). This is the only deterministic
    guard for that cap — otherwise it is purely LLM-behavioral. Advisory: it flags a violation
    for Langfuse without blocking the response."""
    per_prompt_map = {p.prompt_number: p for p in output.per_prompt_analysis}

    capping_issues = [
        issue
        for issue in output.taxonomy_issues
        if issue.category_slug in _SECURITY_SLUGS or issue.severity.strip().lower() == "critical"
    ]
    if not capping_issues:
        return

    overall = output.quality_scores.overall
    if overall > _HARD_CAP_SCORE:
        errors.append(
            ValidationError(
                code="QS_3_HARD_CAP_VIOLATED",
                evaluator_id=EVALUATOR_CALIBRATION,
                severity="advisory",
                detail=(
                    f"overall score ({overall}) exceeds the {_HARD_CAP_SCORE}/10 hard cap despite a "
                    f"security/critical issue ({capping_issues[0].category_slug})"
                ),
                expected=f"overall <= {_HARD_CAP_SCORE}",
                actual=str(overall),
            )
        )

    # The cap also binds the per-prompt score of each prompt a capping issue is attributed to.
    # A prompt not carrying the issue may legitimately score higher, so scope to attributed ones.
    for issue in capping_issues:
        for prompt_num in normalize_prompt_refs(issue.prompts):
            ppa = per_prompt_map.get(prompt_num)
            if ppa is not None and ppa.score > _HARD_CAP_SCORE:
                errors.append(
                    ValidationError(
                        code="QS_3_HARD_CAP_VIOLATED",
                        evaluator_id=EVALUATOR_CALIBRATION,
                        severity="advisory",
                        detail=(
                            f"prompt {prompt_num}: per-prompt score ({ppa.score}) exceeds the "
                            f"{_HARD_CAP_SCORE}/10 hard cap despite an attributed security/critical "
                            f"issue ({issue.category_slug})"
                        ),
                        expected=f"score <= {_HARD_CAP_SCORE}",
                        actual=str(ppa.score),
                    )
                )


def _check_ti2(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """TI-2: every taxonomy issue's category_slug must be a valid enum value (not None)."""
    for issue in output.taxonomy_issues:
        # Pydantic coerces valid CategorySlug enum values; invalid strings arrive as None.
        if issue.category_slug is None:
            errors.append(
                ValidationError(
                    code="TI_2_INVALID_SLUG",
                    evaluator_id=EVALUATOR_ISSUE_DETECTION,
                    severity="blocking",
                    detail="taxonomy issue has category_slug=None (LLM returned unknown slug)",
                    expected=f"one of {sorted(VALID_CATEGORY_SLUGS)}",
                    actual="None",
                )
            )


def _check_tt1(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """TT-1: task_type must be a valid value from the allowed enum set."""
    task_type = getattr(output, "task_type", None)
    if task_type not in VALID_TASK_TYPES:
        errors.append(
            ValidationError(
                code="TT_1_INVALID_TASK_TYPE",
                evaluator_id=EVALUATOR_TASK_CLASS,
                severity="blocking",
                detail=f"task_type '{task_type}' is not in the valid set",
                expected=f"one of {sorted(VALID_TASK_TYPES)}",
                actual=str(task_type),
            )
        )


def _check_rec3(output: AnalyzePromptResponse, errors: list[ValidationError]) -> None:
    """REC-3 (ADVISORY): recommendation count must be between 2 and 5 inclusive.

    The upper bound of 5 mirrors the "at most 5 recommendations" the system prompt instructs
    (analyze_prompt.py). The bound is enforced by the prompt, not the schema — the schema
    deliberately carries no maxItems (it would break gemini-2.5-flash; see schemas/analyze_prompt.py)
    — so a non-compliant response CAN exceed 5, and this advisory surfaces that for Langfuse."""
    count = len(output.recommendations)
    if not (2 <= count <= 5):
        errors.append(
            ValidationError(
                code="REC_3_COUNT_OUT_OF_BOUNDS",
                evaluator_id=EVALUATOR_RECOMMENDATION,
                severity="advisory",
                detail=f"recommendations count ({count}) is outside bounds [2, 5]",
                expected="between 2 and 5",
                actual=str(count),
            )
        )


# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------

# Map of evaluator_id → list of validator functions that contribute to it.
# Order matters within a group: blocking checks run before advisory.
_VALIDATORS = [
    _check_ppa2,
    _check_ppa3,
    _check_ppa1,  # advisory — after blocking PPA checks
    _check_qs1,
    _check_qs2,
    _check_qs3,
    _check_ti2,
    _check_tt1,
    _check_rec3,
]

_ALL_EVALUATOR_IDS = {
    EVALUATOR_PER_PROMPT,
    EVALUATOR_CALIBRATION,
    EVALUATOR_ISSUE_DETECTION,
    EVALUATOR_TASK_CLASS,
    EVALUATOR_RECOMMENDATION,
}


def _is_skill_only(output: AnalyzePromptResponse) -> bool:
    """Whether the conversation carried only skill-authored content (no human-written prompt):
    at least one per-prompt segment exists and none is user-authored. Mirrors the Go-side
    DeriveNoUserPrompt (which persists the marker) — used here only to skip advisory checks that
    do not apply to a skill-only session. No segments at all → False (no provenance signal)."""
    saw_segment = False
    for prompt in output.per_prompt_analysis:
        for seg in prompt.segments:
            saw_segment = True
            if seg.provenance == "user":
                return False
    return saw_segment


def validate_analyze_prompt_output(output: AnalyzePromptResponse) -> ValidationResult:
    """Run all code validators against an AnalyzePromptResponse.

    Never raises — individual validators catch their own exceptions and emit an
    advisory VALIDATOR_INTERNAL_ERROR rather than propagating.

    Returns a ValidationResult with:
      - errors: all failures, both blocking and advisory
      - scores: per-evaluator 0/1 (for Langfuse)
      - has_blocking: True if any BLOCKING error
      - passed: False only if has_blocking
    """
    errors: list[ValidationError] = []

    # A skill-only session (only skills invoked, Y empty) legitimately has an empty/thin
    # per_prompt_analysis and no recommendations — the score-calibration, per-prompt and
    # recommendation-count checks do not apply and would emit spurious advisories. Structural
    # checks (issue slug / task_type validity) still run. Derived from segment provenance — the
    # same rule the Go side uses to persist the marker; the LLM emits no conversation-level flag.
    skip_when_skill_only = {_check_qs1, _check_qs2, _check_ppa1, _check_rec3} if _is_skill_only(output) else set()

    for validator_fn in _VALIDATORS:
        if validator_fn in skip_when_skill_only:
            continue
        try:
            validator_fn(output, errors)
        except Exception as exc:  # noqa: BLE001
            # Validator crashed — emit advisory so it surfaces in Langfuse without breaking anything.
            errors.append(
                ValidationError(
                    code="VALIDATOR_INTERNAL_ERROR",
                    evaluator_id=EVALUATOR_PER_PROMPT,  # generic fallback evaluator
                    severity="advisory",
                    detail=f"Validator {validator_fn.__name__} raised {type(exc).__name__}: {exc}",
                    expected="no exception",
                    actual=type(exc).__name__,
                )
            )

    # Build per-evaluator scores: 0 if any error for that evaluator, else 1.
    failed_evaluators = {e.evaluator_id for e in errors}
    scores = {eid: (0 if eid in failed_evaluators else 1) for eid in _ALL_EVALUATOR_IDS}

    has_blocking = any(e.severity == "blocking" for e in errors)
    has_advisory = any(e.severity == "advisory" for e in errors)

    return ValidationResult(
        passed=not has_blocking,
        errors=errors,
        scores=scores,
        has_blocking=has_blocking,
        has_advisory=has_advisory,
    )
