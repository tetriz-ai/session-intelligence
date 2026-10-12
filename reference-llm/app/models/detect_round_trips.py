# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic request/response models for POST /api/v1/detect-round-trips.

The Go spine sends guard-valid P/A/Q triples; the endpoint scores each with the loaded
MiniLM + NLI detector and returns a per-pair round-trip probability plus the 19 features. The
detector is policy-free — it ranks; the Go E fire-gate decides what to do with the scores.
Mirrors the round-trip detector contract.
"""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field

# Defensive caps — the Go spine already truncates payloads and caps the pair count, and the
# feature extractor truncates again internally; these only bound the work a single request can
# pin on the shared transformer threads.
_MAX_PAIRS = 2000
_MAX_TEXT = 8192


class PairPayload(BaseModel):
    model_config = ConfigDict(extra="forbid")

    prompt_index: int
    P: str = Field(..., max_length=_MAX_TEXT, description="The earlier human prompt of the pair.")
    agent_response: str = Field(..., max_length=_MAX_TEXT, description="Assistant turns between P and Q.")
    Q: str = Field(..., max_length=_MAX_TEXT, description="The later human prompt of the pair.")
    prior: str = Field(default="", max_length=_MAX_TEXT, description="Human turn before P; '' for the first pair.")


class DetectRoundTripsRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    pairs: list[PairPayload] = Field(default_factory=list, max_length=_MAX_PAIRS)


class PairResult(BaseModel):
    prompt_index: int
    round_trip_score: float
    is_round_trip: bool
    features: dict[str, float]


class DetectRoundTripsResponse(BaseModel):
    threshold: float
    encoder: str
    nli_model: str
    results: list[PairResult]
