# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""POST /api/v1/detect-round-trips — the PAIRS detector scoring endpoint.

Policy-free model endpoint: takes the guard-valid P/A/Q triples the Go spine enumerated,
scores each with the resident MiniLM + NLI detector and the fitted classifier, and returns the
round-trip probability + the 19 features per pair. The Go E fire-gate applies the top-N / cap.

The detector is loaded once at startup onto ``app.state.round_trip_detector``. A request that
arrives before it is ready, or in an env where the detector is disabled, gets a 503 — the
consumer treats that as a transient failure and redelivers.
"""

from __future__ import annotations

import time

import structlog
from fastapi import APIRouter, HTTPException, Request
from fastapi.concurrency import run_in_threadpool
from structlog.contextvars import bind_contextvars

from app.metrics import (
    DETECT_ROUND_TRIPS_DURATION,
    DETECT_ROUND_TRIPS_PAIRS,
    DETECT_ROUND_TRIPS_STATUS_ERROR,
    DETECT_ROUND_TRIPS_STATUS_SUCCESS,
    DETECT_ROUND_TRIPS_TOTAL,
)
from app.ml_model_inference.detectors.round_trip import PairInput, RoundTripDetector
from app.models.detect_round_trips import (
    DetectRoundTripsRequest,
    DetectRoundTripsResponse,
    PairResult,
)

logger = structlog.get_logger(__name__)

router = APIRouter(prefix="/api/v1", tags=["detect-round-trips"])


@router.post("/detect-round-trips", response_model=DetectRoundTripsResponse)
async def detect_round_trips(request: DetectRoundTripsRequest, req: Request) -> DetectRoundTripsResponse:
    detector: RoundTripDetector | None = getattr(req.app.state, "round_trip_detector", None)
    if detector is None:
        logger.error("round-trip detector not loaded; service warming up or disabled — 503 to caller")
        raise HTTPException(
            status_code=503,
            detail="round-trip detector not initialized; service is starting up or disabled.",
        )

    bind_contextvars(pair_count=len(request.pairs))
    pairs = [
        PairInput(
            prompt_index=p.prompt_index,
            P=p.P,
            agent_response=p.agent_response,
            Q=p.Q,
            prior=p.prior,
        )
        for p in request.pairs
    ]

    start = time.perf_counter()
    try:
        with DETECT_ROUND_TRIPS_DURATION.time():
            results = await run_in_threadpool(detector.score, pairs)
    except Exception:
        DETECT_ROUND_TRIPS_TOTAL.labels(DETECT_ROUND_TRIPS_STATUS_ERROR).inc()
        logger.exception("detect-round-trips failed while scoring; returning 500 to caller")
        raise

    DETECT_ROUND_TRIPS_TOTAL.labels(DETECT_ROUND_TRIPS_STATUS_SUCCESS).inc()
    DETECT_ROUND_TRIPS_PAIRS.inc(len(results))
    logger.info(
        "detect-round-trips request completed",
        scored=len(results),
        elapsed_s=round(time.perf_counter() - start, 3),
    )
    return DetectRoundTripsResponse(
        threshold=detector.threshold,
        encoder=detector.encoder_id,
        nli_model=detector.nli_id,
        results=[PairResult(**result) for result in results],
    )
