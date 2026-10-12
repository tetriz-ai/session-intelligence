# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Request body size limit (OOM guard).

A pure-ASGI middleware that rejects an oversized request body with 413 before it is
buffered into memory. Two layers: a fast reject when the client declares an oversized
``Content-Length``, and a streaming byte counter that aborts a chunked body (no declared
length) once it exceeds the cap. The per-field ``content`` cap in
``app/models/generate_content.py`` remains the precise bound; this is the coarse guard that
stops a pathological upload from exhausting memory.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Any

# Ceiling on a single request body. Comfortably above the largest legitimate request
# (the generate-content ``content`` field is capped at MAX_CONTENT_CHARS = 3,000,000 chars
# plus session_context), while bounding memory against a pathological upload.
MAX_REQUEST_BODY_BYTES = 16 * 1024 * 1024  # 16 MiB

Scope = dict[str, Any]
Message = dict[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]


class _BodyTooLargeError(Exception):
    """Raised from the wrapped receive when the streamed body exceeds the cap."""


class BodySizeLimitMiddleware:
    def __init__(self, app: Callable[..., Awaitable[None]], max_body_bytes: int = MAX_REQUEST_BODY_BYTES) -> None:
        self.app = app
        self.max_body_bytes = max_body_bytes

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return

        # Fast path: the client declared an oversized body.
        for name, value in scope.get("headers", []):
            if name == b"content-length":
                try:
                    declared = int(value)
                except ValueError:
                    break
                if declared > self.max_body_bytes:
                    await self._reject(send)
                    return
                break

        received = 0
        response_started = False

        async def tracking_send(message: Message) -> None:
            nonlocal response_started
            if message["type"] == "http.response.start":
                response_started = True
            await send(message)

        async def capped_receive() -> Message:
            nonlocal received
            message = await receive()
            if message["type"] == "http.request":
                received += len(message.get("body", b""))
                if received > self.max_body_bytes:
                    raise _BodyTooLargeError
            return message

        try:
            await self.app(scope, capped_receive, tracking_send)
        except _BodyTooLargeError:
            if response_started:
                # Every endpoint here reads the full body before responding, so this is unreachable
                # in practice — but if an endpoint ever streamed a response while still consuming the
                # body, a second http.response.start would be an ASGI error. Abort the connection
                # instead of emitting a malformed 413.
                raise
            await self._reject(send)

    async def _reject(self, send: Send) -> None:
        await send(
            {
                "type": "http.response.start",
                "status": 413,
                "headers": [(b"content-type", b"application/json")],
            }
        )
        await send(
            {
                "type": "http.response.body",
                "body": b'{"detail":"request body too large"}',
            }
        )
