# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
FastAPI application entry point for the reference-llm service.

Startup:
  - Load config from YAML + env vars
  - Configure structured logging
  - Initialise the per-tenant Gemini provider factory
  - Load the round-trip detector when enabled
  - Register routes and middleware

Shutdown:
  - Close LLM provider clients
  - Graceful cleanup via lifespan context manager
"""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import structlog
from fastapi import FastAPI
from fastapi.exceptions import RequestValidationError
from prometheus_fastapi_instrumentator import Instrumentator
from starlette.requests import Request
from starlette.responses import JSONResponse

from app.api.detect_round_trips import router as detect_round_trips_router
from app.api.generate_content import router as generate_content_router
from app.api.health import router as health_router
from app.config.config import Environment, get_config
from app.config.logging import setup_logging
from app.llm import tracing
from app.llm.providers.provider_factory import (
    ProviderFactory,
    clear_provider_factory,
    set_provider_factory,
)
from app.middleware.access_log import AccessLogMiddleware
from app.middleware.body_limit import MAX_REQUEST_BODY_BYTES, BodySizeLimitMiddleware
from app.ml_model_inference.detectors.round_trip.loader import load_detector

logger = structlog.get_logger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    """Application lifespan: startup and shutdown hooks."""
    config = get_config()
    setup_logging()

    # Initialise the LLM provider factory from the single configured Gemini key
    # (LLM__GEMINI_API_KEY) — bring your own Gemini key and the service runs in EVERY
    # environment. When the key is absent the factory is still built (with a placeholder) so
    # the service boots and health checks pass; LLM calls then fail until the key is set.
    api_key = config.llm.GEMINI_API_KEY
    if not api_key:
        api_key = "not-configured"
        logger.warning(
            "Gemini API key not configured; LLM calls will fail until LLM__GEMINI_API_KEY is set",
            hint="Set LLM__GEMINI_API_KEY env var to enable LLM calls",
        )
    factory = ProviderFactory(api_key=api_key, timeout=config.llm.REQUEST_TIMEOUT)
    set_provider_factory(factory)
    logger.info("provider factory initialized")

    # Load the round-trip PAIRS detector (its own MiniLM + NLI + scaler/logreg head) once at
    # startup, when enabled. DETECTOR_ENABLED arms the load; a missing/corrupt bundle or a
    # transformer-revision mismatch raises here and fails the pod rather than serving drift.
    # Outside development the baked bundle is required. The shipped classifier is an uncalibrated
    # placeholder, so DETECTOR_ENABLED defaults to false and /detect-round-trips returns 503.
    if config.ml_model_inference.DETECTOR_ENABLED:
        if config.ENVIRONMENT != Environment.DEVELOPMENT and not config.ml_model_inference.ARTIFACT_ROOT:
            raise RuntimeError(
                "ml_model_inference.ARTIFACT_ROOT is required when DETECTOR_ENABLED outside development. "
                "Point it at the baked detector bundle, or keep DETECTOR_ENABLED=false."
            )
        app.state.round_trip_detector = load_detector(config.ml_model_inference.ARTIFACT_ROOT)
    else:
        app.state.round_trip_detector = None
        logger.info(
            "round-trip detector disabled by config; /detect-round-trips will return 503",
            hint="Bake the detector bundle, point ARTIFACT_ROOT at it, and set DETECTOR_ENABLED=true",
        )

    # Initialise Langfuse observability (self-hosted).
    # tracing.configure() sets up the singleton that @observe decorators use.
    # On auth failure it explicitly disables the SDK so decorators become no-ops.
    if config.langfuse.ENABLED:
        tracing.configure(
            public_key=config.langfuse.PUBLIC_KEY,
            secret_key=config.langfuse.SECRET_KEY,
            host=config.langfuse.HOST,
        )
    else:
        logger.info(
            "langfuse tracing disabled by config; LLM completions will not be recorded",
            hint="Set langfuse.ENABLED to true and configure keys to enable tracing",
        )
        # Register a tracing-disabled client so @observe / get_client() don't
        # auto-init a keyless client on first LLM call (which logs an auth error +
        # "no active span" on every request). See configure_disabled().
        tracing.configure_disabled()

    logger.info(
        "reference-llm starting",
        environment=config.ENVIRONMENT.value,
        port=config.server.PORT,
        workers=config.server.WORKERS,
    )
    yield

    tracing.shutdown()
    await factory.close_all()
    clear_provider_factory()
    logger.info("reference-llm shutting down")


def create_app() -> FastAPI:
    config = get_config()

    application = FastAPI(
        title="reference-llm",
        description="Reference LLM processing service for session-intelligence",
        version="0.1.0",
        docs_url="/docs" if config.ENVIRONMENT == "development" else None,
        redoc_url=None,
        lifespan=lifespan,
    )

    application.add_middleware(AccessLogMiddleware)
    # Reject an oversized request body before it is buffered, so a huge upload cannot
    # exhaust memory. The per-field content cap (models/generate_content.py) is the precise
    # bound; this is the coarse outer OOM guard.
    application.add_middleware(BodySizeLimitMiddleware, max_body_bytes=MAX_REQUEST_BODY_BYTES)

    @application.exception_handler(RequestValidationError)
    async def validation_error_handler(_request: Request, exc: RequestValidationError) -> JSONResponse:
        """Strip user input from validation errors to avoid echoing sensitive data.

        Drops both ``input`` (the offending value) and ``ctx`` — for a custom
        ``field_validator`` ``ctx`` holds the raised ``ValueError`` object, which
        is not JSON-serializable and would 500 the error handler itself; it can
        also carry input fragments, so dropping it matches the input-stripping intent.
        """
        errors = []
        for err in exc.errors():
            errors.append({k: v for k, v in err.items() if k not in ("input", "ctx")})
        return JSONResponse(status_code=422, content={"detail": errors})

    application.include_router(health_router)
    application.include_router(generate_content_router)
    application.include_router(detect_round_trips_router)

    Instrumentator(
        should_group_status_codes=True,
        excluded_handlers=["/_health", "/readyz", "/metrics"],
    ).instrument(application).expose(application, endpoint="/metrics")

    return application


app = create_app()


def main() -> None:
    """Run the service with uvicorn, honoring the configured host/port/log level.

    SERVER__PORT (and SERVER__HOST, SERVER__WORKERS, SERVER__LOG_LEVEL) override the
    listen settings via the config layer. Used by the Dockerfile entrypoint.
    """
    import uvicorn

    config = get_config()
    uvicorn.run(
        "app.main:app",
        host=config.server.HOST,
        port=config.server.PORT,
        workers=config.server.WORKERS,
        log_level=config.server.LOG_LEVEL.lower(),
    )


if __name__ == "__main__":
    main()
