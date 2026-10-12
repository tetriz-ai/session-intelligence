# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
Access log middleware -- logs every HTTP request as structured JSON.

Skips health check endpoints to avoid noise.
"""

from __future__ import annotations

import time
import uuid

import structlog
from starlette.middleware.base import BaseHTTPMiddleware, RequestResponseEndpoint
from starlette.requests import Request
from starlette.responses import Response
from structlog.contextvars import bind_contextvars

logger = structlog.get_logger("access")

_SKIP_PATHS = {"/_health", "/readyz"}


class AccessLogMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request: Request, call_next: RequestResponseEndpoint) -> Response:
        # Clear per-request context vars at the start of each request to prevent
        # stale org_id/source_id from a previous request leaking into this one.
        structlog.contextvars.clear_contextvars()

        # Always generate a fresh trace_id — correlates all Python logs for this request.
        ctx_vars: dict = {"trace_id": str(uuid.uuid4())}
        # If the caller (e.g. Go the engine) passes its trace_id via X-Trace-ID,
        # capture it as origin_trace_id so logs can be correlated across services.
        origin_trace_id = request.headers.get("x-trace-id")
        if origin_trace_id:
            ctx_vars["origin_trace_id"] = origin_trace_id
        bind_contextvars(**ctx_vars)

        if request.url.path in _SKIP_PATHS:
            return await call_next(request)

        start = time.perf_counter()
        response = await call_next(request)
        duration_ms = (time.perf_counter() - start) * 1000

        logger.info(
            "http request handled",
            method=request.method,
            url=request.url.path,
            status=response.status_code,
            timeUsedMs=round(duration_ms, 2),
            client=request.client.host if request.client else None,
        )
        return response
