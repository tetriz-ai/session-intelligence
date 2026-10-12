# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
Health check endpoints.

- /_health  : Liveness probe (always 200 if process is up)
- /readyz   : Readiness probe (checks dependencies)
"""

from fastapi import APIRouter

from app.llm.providers.provider_factory import is_factory_initialized
from app.models.health import HealthResponse, ReadinessResponse

router = APIRouter(tags=["health"])


@router.get("/_health", response_model=HealthResponse)
async def liveness() -> HealthResponse:
    return HealthResponse(status="ok")


@router.get("/readyz", response_model=ReadinessResponse)
async def readiness() -> ReadinessResponse:
    factory_ready = is_factory_initialized()
    checks = {"provider_factory": "ok" if factory_ready else "not_initialized"}
    overall = "ok" if factory_ready else "degraded"
    return ReadinessResponse(status=overall, checks=checks)
