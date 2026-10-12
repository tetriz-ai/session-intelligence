# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""System prompts for each task type."""

from app.llm.prompts.analyze_prompt import ANALYZE_PROMPT_SYSTEM
from app.llm.prompts.loops import (
    LOOP_ANALYSIS_ADDENDUM,
    LOOP_ANALYSIS_SYSTEM,
    LOOPS_ONLY_ADDENDUM,
)
from app.llm.prompts.session_analysis import (
    ANALYZE_DISCARD_SYSTEM,
    ANALYZE_EVIDENCE_SYSTEM,
    ANALYZE_READ_SYSTEM,
)

__all__ = [
    "ANALYZE_DISCARD_SYSTEM",
    "ANALYZE_EVIDENCE_SYSTEM",
    "ANALYZE_PROMPT_SYSTEM",
    "ANALYZE_READ_SYSTEM",
    "LOOP_ANALYSIS_SYSTEM",
    "LOOPS_ONLY_ADDENDUM",
    "LOOP_ANALYSIS_ADDENDUM",
]
