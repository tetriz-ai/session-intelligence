# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""LLM error hierarchy.

Provider implementations catch SDK-specific exceptions and re-raise as these.
API handlers map these to HTTP status codes.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from app.llm.types import TokenUsage


class LLMError(Exception):
    """Base exception for LLM operations."""

    # Overridden per subclass; travels in to_detail() as the wire contract.
    error_code = "LLM_ERROR"

    def __init__(
        self,
        message: str,
        provider: str,
        usage: TokenUsage | None = None,
        model: str | None = None,
    ):
        self.provider = provider
        # Token usage from a response that was received but rejected (e.g. a
        # MAX_TOKENS truncation or safety block). Those calls are still billed —
        # including their thinking tokens — so the API handler emits the token
        # metrics from here. None when no response was received (transport error).
        self.usage = usage
        # The model that produced `usage` — set alongside usage on billed
        # rejections so the token/cost metrics attribute them to the right model
        # rate (which, under 2.5→3.5 fallback, may differ from the task's default).
        # None when no billed response was received.
        self.model = model
        super().__init__(message)

    def to_detail(self) -> dict[str, Any]:
        """Structured HTTPException detail preserving billed-call metadata.

        Every LLMError-derived 502 serializes through here, so one shape covers
        all of them; `usage` is absent (not null) when the call was never billed.
        """
        d: dict[str, Any] = {
            "error": str(self),
            # A stable identifier for the failure class, so a caller can branch on the
            # KIND of failure without matching on prose. The message is written for a
            # human reading a log; a consumer that sniffs it breaks the day it is reworded,
            # and the Go caller previously had nothing else to go on.
            "code": type(self).error_code,
            "provider": self.provider,
            "model": self.model,
        }
        if self.usage is not None:
            d["usage"] = self.usage.model_dump()
        return d


class LLMRateLimitError(LLMError):
    """Provider rate limit exceeded."""

    error_code = "RATE_LIMIT"


class LLMAuthenticationError(LLMError):
    """Invalid API key or authentication failure."""

    error_code = "AUTHENTICATION"


class LLMInputTooLargeError(LLMError):
    """Input exceeds the model's maximum context length."""

    error_code = "INPUT_TOO_LARGE"


class LLMInvalidRequestError(LLMError):
    """Deterministic INVALID_ARGUMENT fault in the request or its response config —
    e.g. a response schema too complex for the provider to serve ("too many states
    for serving"), or an unsupported parameter. The same request would fail
    identically on a retry or a different model, so the handler returns it as a
    non-retryable 400 rather than a 502 the caller would re-drive in a loop."""

    error_code = "INVALID_REQUEST"


class LLMUnavailableError(LLMError):
    """Availability-class failure: transport/connection error, request timeout, HTTP
    5xx, or model-not-found (404 / NOT_FOUND).

    Distinct from the generic LLMError (which also covers deterministic response faults
    like a MAX_TOKENS truncation) so the model fallback can retry only on an unavailable
    primary — the retry policy itself lives in gemini_fallback.py.
    """

    error_code = "UNAVAILABLE"


class LLMSafetyBlockedError(LLMError):
    """Provider's safety filter blocked the response.

    Kept distinct from LLMError so the request handler can record
    `status=safety_blocked` on the LLM call counter without it also
    landing in the generic `status=error` series via the `except LLMError`
    catch-all. Carries its own error_code so the wire `code` identifies the
    safety block rather than inheriting the generic "LLM_ERROR".
    """

    error_code = "SAFETY_BLOCKED"


class LLMOutputTruncatedError(LLMError):
    """The model filled its output budget before finishing the response.

    Deterministic, and that is the whole point of separating it: the same input yields the
    same truncation on every attempt, so a caller that treats it as a transient failure pays
    the call again to reach an identical outcome. On a thinking model the budget is shared —
    thinking tokens bill against max_output_tokens — so a large thinking_budget shrinks the
    room left for the body, and the fix is to bound what the response must contain rather
    than to raise a ceiling that may already be at the model's maximum.
    """

    error_code = "OUTPUT_TRUNCATED"
