# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Abstract base class for LLM provider adapters."""

from __future__ import annotations

from abc import ABC, abstractmethod

from app.llm.types import CompletionRequest, CompletionResponse


class LLMProvider(ABC):
    """Interface that every LLM provider must implement."""

    @abstractmethod
    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        """Execute a chat/completion call."""
        ...

    @abstractmethod
    async def close(self) -> None:
        """Release resources (HTTP clients, etc.)."""
        ...

    @property
    def cache_scope(self) -> str:
        """Identifies the credential this provider authenticates as, for the system-prompt cache
        key (a cache is only valid for the key that created it). Empty for providers that do not
        use the shared prompt cache; Gemini overrides it with a digest of its api_key."""
        return ""
