# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic response models for the liveness (/_health) and readiness (/readyz) probes."""

from __future__ import annotations

from pydantic import BaseModel, Field


class HealthResponse(BaseModel):
    """Liveness: the process is up. `status` is "ok" whenever the handler runs."""

    status: str


class ReadinessResponse(BaseModel):
    """Readiness: `status` is the overall verdict and `checks` maps each dependency to its state."""

    status: str
    checks: dict[str, str] = Field(default_factory=dict)
