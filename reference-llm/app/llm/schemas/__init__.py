# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic schemas for structured LLM output.

Split by task type — one module per task. Re-exported here so existing
imports (`from app.llm.schemas import X`) keep working.
"""

from app.llm.schemas.analyze_prompt import (
    AnalyzePromptQualityResponse,
    AnalyzePromptResponse,
    CategorySlug,
    InputContract,
    InvokedSkill,
    PerPromptAnalysis,
    PromptSegment,
    Provenance,
    QualityScores,
    Source,
    TaxonomyIssue,
    TokenAnalysis,
    TokenBreakdown,
)
from app.llm.schemas.loops import (
    AnalyzeLoopsResponse,
    EventCoverage,
    EventLocator,
    LoopAnalysis,
    LoopCheckAnalysis,
    LoopCheckIndependence,
    LoopCheckKind,
    LoopCheckOutcome,
    LoopInitiatorKind,
    LoopIterationActionKind,
    LoopIterationAnalysis,
    LoopRunAnalysis,
    LoopRunStatus,
)

# Only the three response models are re-exported here; the session-analysis enum
# vocabulary lives in app.llm.schemas.session_analysis and is imported from there
# directly (several names — InputContract, Severity — would otherwise collide with
# the analyze_prompt exports above).
from app.llm.schemas.session_analysis import (
    AnalyzeDiscardResponse,
    AnalyzeEvidenceResponse,
    AnalyzeReadResponse,
)

__all__ = [
    "AnalyzeDiscardResponse",
    "AnalyzeEvidenceResponse",
    "AnalyzeLoopsResponse",
    "AnalyzeReadResponse",
    "AnalyzePromptQualityResponse",
    "AnalyzePromptResponse",
    "CategorySlug",
    "EventCoverage",
    "EventLocator",
    "InputContract",
    "InvokedSkill",
    "LoopAnalysis",
    "LoopCheckAnalysis",
    "LoopCheckIndependence",
    "LoopCheckKind",
    "LoopCheckOutcome",
    "LoopInitiatorKind",
    "LoopIterationActionKind",
    "LoopIterationAnalysis",
    "LoopRunAnalysis",
    "LoopRunStatus",
    "PerPromptAnalysis",
    "PromptSegment",
    "Provenance",
    "QualityScores",
    "Source",
    "TaxonomyIssue",
    "TokenAnalysis",
    "TokenBreakdown",
]
