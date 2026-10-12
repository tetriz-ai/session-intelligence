# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Single-key LLM provider factory.

Holds one GeminiProvider built from the single configured Gemini API key
(LLM__GEMINI_API_KEY). The factory indirection is kept so endpoints resolve their
provider through one injectable seam (FastAPI Depends), which the tests override.
"""

from __future__ import annotations

import structlog

from app.llm.providers.base import LLMProvider
from app.llm.providers.gemini import GeminiProvider

logger = structlog.get_logger(__name__)

# Module-level singleton, following the same pattern as get_config().
_factory: ProviderFactory | None = None


class ProviderFactory:
    """Owns the single GeminiProvider the service serves every request with."""

    def __init__(self, api_key: str, timeout: int) -> None:
        self._provider: LLMProvider = GeminiProvider(api_key=api_key, timeout=timeout)

    def get_provider(self, org_id: str = "") -> LLMProvider:
        """Return the configured provider. org_id is accepted for call-site
        compatibility but no longer selects a per-tenant key."""
        return self._provider

    async def close_all(self) -> None:
        """Close the provider, logging and swallowing a close failure so shutdown
        never raises."""
        try:
            await self._provider.close()
        except Exception as exc:
            logger.error(
                "failed to close LLM provider during shutdown",
                error=str(exc),
            )


def get_provider_factory() -> ProviderFactory:
    """FastAPI Depends() compatible getter for the singleton factory."""
    if _factory is None:
        raise RuntimeError("ProviderFactory not initialized. Is the app lifespan running?")
    return _factory


def set_provider_factory(factory: ProviderFactory) -> None:
    """Set the module-level singleton. Called from the FastAPI lifespan."""
    global _factory
    _factory = factory


def clear_provider_factory() -> None:
    """Clear the module-level singleton. Called on shutdown."""
    global _factory
    _factory = None


def is_factory_initialized() -> bool:
    """Check if the factory singleton has been set. Used by readiness probes."""
    return _factory is not None
