# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

from typing import Any

from pydantic import BaseModel, Field

from app.llm.router import MAX_CONTENT_CHARS
from app.llm.types import TokenUsage


class GenerateContentRequest(BaseModel):
    content: str = Field(..., min_length=1, max_length=MAX_CONTENT_CHARS, description="User content / question")
    task_type: str = Field(..., min_length=1, description="Task type for prompt routing (e.g. 'analyze_prompt')")
    org_id: str = Field(..., min_length=1, description="Organization ID for per-tenant trace differentiation")
    source_id: str = Field(
        ...,
        min_length=1,
        description=(
            "Identifier of the source backend entity this LLM call is being made "
            "for — bound into structlog context and Langfuse trace metadata for "
            "per-request debugging correlation. Required: every call must "
            "reference a specific entity. analyze_prompt passes the "
            "raw_record_id; aggregate task types like issue_quality_check "
            "pass a domain-specific ID (e.g. issue_key)."
        ),
    )
    temperature: float | None = Field(None, ge=0.0, le=2.0, description="Override task default temperature")
    max_tokens: int | None = Field(None, ge=1, le=65_536, description="Override task default max_tokens")
    session_context: dict[str, Any] | None = Field(
        None,
        description=("IDE session context for prompt analysis (forwarded from UsageMetadata proto)"),
    )
    use_system_prompt_cache: bool | None = Field(
        None,
        description=(
            "Opt in/out of Gemini system-prompt caching, tri-state so an explicit "
            "choice always wins over the per-task default. None (unset) → the endpoint's "
            "default: ON for the session-analysis producers, OFF for every other task. "
            "True/False force it on/off — a caller can turn caching OFF on a session-analysis "
            "task (the rollback path) by sending False. Engages only when the call routes to "
            "Gemini and a system prompt is present."
        ),
    )


class ValidationErrorResponse(BaseModel):
    code: str
    evaluator_id: str
    severity: str
    detail: str
    expected: str
    actual: str


class InsightsMetadataResponse(BaseModel):
    errors: list[ValidationErrorResponse] = []
    has_blocking: bool = False
    error_count: int = 0


class GenerateContentResponse(BaseModel):
    result: str = Field(..., description="Generated content")
    task_type: str = Field(..., description="Task type used")
    model: str = Field(..., description="Model used")
    provider: str = Field(..., description="Provider used")
    usage: TokenUsage | None = Field(None, description="Token usage statistics")
    # Estimated USD cost of this call, from the per-model rate card.
    # None for models with no vendor bill (local ML models). Go forwards this onto
    # the per-call cost event.
    cost_usd: float | None = Field(None, description="Estimated USD cost; null when the model is unpriced.")
    metadata: InsightsMetadataResponse | None = Field(None, description="Validation metadata (analyze_prompt only)")
