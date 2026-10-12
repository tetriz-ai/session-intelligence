# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Unit tests for the analyze_prompt runtime output validators. Hermetic — pure functions
over constructed AnalyzePromptResponse objects, no network or LLM."""

from app.llm.schemas.analyze_prompt import (
    AnalyzePromptResponse,
    PerPromptAnalysis,
    QualityScores,
    TaxonomyIssue,
    TokenAnalysis,
)
from app.tasks.analyze_prompt.validators import validate_analyze_prompt_output


def _quality_scores(overall: int = 8, dim: int = 8) -> QualityScores:
    return QualityScores(
        overall=overall,
        clarity=dim,
        specificity=dim,
        context_provision=dim,
        actionability=dim,
        completeness=dim,
        efficiency=dim,
    )


def _token_analysis() -> TokenAnalysis:
    return TokenAnalysis(
        total_tokens=100,
        wasted_tokens=0,
        wasted_percentage=0.0,
        breakdown=[],
        session_limit_hit_count=0,
    )


def _ppa(prompt_number: int, score: int, issues: str = "none") -> PerPromptAnalysis:
    return PerPromptAnalysis(
        prompt_number=prompt_number,
        score=score,
        feedback="ok",
        issues=issues,
        context_reliance="N/A",
        quality_practices="N/A",
        files_modified="N/A",
        lines_changed="N/A",
        improved_prompt="N/A",
        discard_probability=0.0,
        preceding_pair_tokens=0,
    )


def _response(
    *,
    quality: QualityScores,
    taxonomy: list[TaxonomyIssue],
    ppa: list[PerPromptAnalysis],
    task_type: str = "feature",
    recommendations: list[str] | None = None,
) -> AnalyzePromptResponse:
    return AnalyzePromptResponse(
        quality_scores=quality,
        taxonomy_issues=taxonomy,
        token_analysis=_token_analysis(),
        improved_prompt="N/A",
        recommendations=recommendations if recommendations is not None else ["do x", "do y"],
        per_prompt_analysis=ppa,
        task_type=task_type,
        ai_reviewed=False,
    )


def test_clean_response_passes_with_no_blocking():
    out = _response(
        quality=_quality_scores(overall=8, dim=8),
        taxonomy=[],
        ppa=[_ppa(1, 8)],
    )
    result = validate_analyze_prompt_output(out)
    assert result.passed is True
    assert result.has_blocking is False


def test_invalid_task_type_is_blocking():
    out = _response(
        quality=_quality_scores(),
        taxonomy=[],
        ppa=[_ppa(1, 8)],
        task_type="not-a-real-task-type",
    )
    result = validate_analyze_prompt_output(out)
    assert result.has_blocking is True
    assert result.passed is False
    assert any(e.code == "TT_1_INVALID_TASK_TYPE" for e in result.errors)


def test_slug_not_in_taxonomy_is_blocking():
    # A per-prompt issue slug with no matching taxonomy_issues entry (PPA-2).
    out = _response(
        quality=_quality_scores(),
        taxonomy=[],
        ppa=[_ppa(1, 5, issues="ambiguous-directive")],
    )
    result = validate_analyze_prompt_output(out)
    assert result.has_blocking is True
    assert any(e.code == "PPA_2_SLUG_NOT_IN_TAXONOMY" for e in result.errors)


def test_security_hard_cap_violation_is_flagged_advisory():
    # A security-layer issue caps overall at 3; an overall of 9 violates it (QS-3 advisory).
    security_issue = TaxonomyIssue(
        category_slug="api-key-leak",
        severity="high",
        layer="security",
        title="leaked key",
        description="a real key was pasted",
        suggestion="rotate it",
        affected_text="[redacted]",
        estimated_wasted_tokens=0,
        prompts="1",
        action_item="Rotate the leaked key.",
    )
    out = _response(
        quality=_quality_scores(overall=9, dim=9),
        taxonomy=[security_issue],
        ppa=[_ppa(1, 9, issues="api-key-leak")],
    )
    result = validate_analyze_prompt_output(out)
    assert any(e.code == "QS_3_HARD_CAP_VIOLATED" for e in result.errors)
    # QS-3 is advisory, so it does not block on its own.
    assert result.has_advisory is True


def test_scores_populated_for_every_evaluator():
    out = _response(
        quality=_quality_scores(),
        taxonomy=[],
        ppa=[_ppa(1, 8)],
    )
    result = validate_analyze_prompt_output(out)
    # Every registered evaluator gets a 0/1 score, never a missing key.
    assert set(result.scores.values()) <= {0, 1}
    assert len(result.scores) >= 5
