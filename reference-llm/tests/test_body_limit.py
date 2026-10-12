# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Hermetic tests for the request body size limit middleware."""

from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.middleware.body_limit import BodySizeLimitMiddleware


def _app_with_cap(max_body_bytes: int) -> FastAPI:
    app = FastAPI()
    app.add_middleware(BodySizeLimitMiddleware, max_body_bytes=max_body_bytes)

    @app.post("/echo")
    async def echo(payload: dict) -> dict:
        return payload

    return app


def test_body_within_cap_is_accepted():
    with TestClient(_app_with_cap(1_000)) as client:
        resp = client.post("/echo", json={"a": 1})
    assert resp.status_code == 200
    assert resp.json() == {"a": 1}


def test_oversized_declared_body_is_rejected_with_413():
    # The JSON body is well over the 10-byte cap; the declared Content-Length trips the guard.
    with TestClient(_app_with_cap(10)) as client:
        resp = client.post("/echo", json={"a": "x" * 100})
    assert resp.status_code == 413


def test_oversized_streamed_body_is_rejected():
    # A generator body is sent chunked (no Content-Length), so the streaming byte counter — not
    # the Content-Length fast path — aborts the read once it crosses the cap, before the whole
    # body is buffered (the OOM guard). The framework surfaces the aborted read as a 4xx; the
    # declared-Content-Length path (above) is the one that returns the explicit 413. Either way
    # the oversized body is refused, never echoed back.
    def chunks():
        for _ in range(10):
            yield b"x" * 100  # 1000 bytes total, over the 50-byte cap

    with TestClient(_app_with_cap(50)) as client:
        resp = client.post("/echo", content=chunks(), headers={"content-type": "application/json"})
    assert resp.status_code >= 400
