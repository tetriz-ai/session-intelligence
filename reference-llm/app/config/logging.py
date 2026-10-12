# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""
Structured logging setup via structlog.

- DEPLOY: JSON lines (machine-parseable)
- DEVELOPMENT: colored, human-readable console output
"""

from __future__ import annotations

import logging
import sys

import structlog

from app.config.config import Environment, get_config


def setup_logging() -> None:
    config = get_config()

    shared_processors: list[structlog.types.Processor] = [
        structlog.contextvars.merge_contextvars,
        structlog.stdlib.add_logger_name,
        structlog.stdlib.add_log_level,
        structlog.processors.TimeStamper(fmt="iso"),
        structlog.processors.StackInfoRenderer(),
        structlog.processors.UnicodeDecoder(),
    ]

    if config.ENVIRONMENT in (Environment.DEPLOY, Environment.PROD):
        renderer: structlog.types.Processor = structlog.processors.JSONRenderer()
        # Rename structlog's `event` key to `msg` so the log message lands in the
        # indexed `msg` field of a log index that does not accept new fields.
        formatter_processors: list[structlog.types.Processor] = [
            structlog.stdlib.ProcessorFormatter.remove_processors_meta,
            structlog.processors.EventRenamer("msg"),
            renderer,
        ]
    else:
        renderer = structlog.dev.ConsoleRenderer()
        formatter_processors = [
            structlog.stdlib.ProcessorFormatter.remove_processors_meta,
            renderer,
        ]

    structlog.configure(
        processors=[
            *shared_processors,
            structlog.stdlib.ProcessorFormatter.wrap_for_formatter,
        ],
        logger_factory=structlog.stdlib.LoggerFactory(),
        wrapper_class=structlog.stdlib.BoundLogger,
        cache_logger_on_first_use=True,
    )

    formatter = structlog.stdlib.ProcessorFormatter(processors=formatter_processors)

    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(formatter)

    root_logger = logging.getLogger()
    root_logger.handlers.clear()
    root_logger.addHandler(handler)
    root_logger.setLevel(config.server.LOG_LEVEL.upper())

    # Suppress noisy third-party loggers
    logging.getLogger("uvicorn.access").setLevel(logging.WARNING)
