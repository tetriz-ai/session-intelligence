# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Hermetic tests for the /api/v1/generate-content endpoint.

No network and no real provider key: a fake LLMProvider is injected via the
get_provider_factory dependency override, so the full endpoint path (routing,
model fallback, error mapping, response shaping) runs offline.
"""

import os

os.environ.setdefault("ENVIRONMENT", "development")

import pytest
from fastapi.testclient import TestClient

from app.llm.exceptions import LLMAuthenticationError, LLMInvalidRequestError, LLMUnavailableError
from app.llm.providers.base import LLMProvider
from app.llm.providers.provider_factory import get_provider_factory
from app.llm.types import CompletionRequest, CompletionResponse, TokenUsage
from app.main import app

# analyze_read is a session-analysis producer pinned to gemini-3.7-flash with a
# gemini-2.5-flash availability fallback — convenient for exercising the fallback path.
_PRIMARY_MODEL = "gemini-3.7-flash"
_FALLBACK_MODEL = "gemini-2.5-flash"


class FakeProvider(LLMProvider):
    """A provider whose completion behaviour is scripted per test.

    ``behaviour`` is called with the CompletionRequest and returns a CompletionResponse
    or raises an LLMError, so a test can model success, a deterministic error, or a
    model-specific availability failure.
    """

    def __init__(self, behaviour) -> None:
        self.behaviour = behaviour
        self.calls: list[str] = []

    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        self.calls.append(request.model)
        return self.behaviour(request)

    async def close(self) -> None:
        return None


class FakeFactory:
    def __init__(self, provider: LLMProvider) -> None:
        self._provider = provider

    def get_provider(self, org_id: str = "") -> LLMProvider:
        return self._provider


def _ok(content: str):
    def behaviour(request: CompletionRequest) -> CompletionResponse:
        return CompletionResponse(
            content=content,
            model=request.model,
            provider="gemini",
            usage=TokenUsage(prompt_tokens=10, completion_tokens=5, total_tokens=15),
        )

    return behaviour


def _request_body(task_type: str = "analyze_read", content: str = "a tiny session transcript") -> dict:
    return {
        "content": content,
        "task_type": task_type,
        "org_id": "org_test",
        "source_id": "rec_1",
    }


@pytest.fixture
def client_with_provider():
    """A TestClient whose generate-content provider is a scripted fake."""
    holder: dict[str, FakeProvider] = {}

    def _install(behaviour) -> FakeProvider:
        provider = FakeProvider(behaviour)
        holder["provider"] = provider
        app.dependency_overrides[get_provider_factory] = lambda: FakeFactory(provider)
        return provider

    with TestClient(app) as client:
        yield client, _install
    app.dependency_overrides.pop(get_provider_factory, None)


def test_generate_content_happy_path(client_with_provider):
    client, install = client_with_provider
    install(_ok('{"ok": true}'))

    resp = client.post("/api/v1/generate-content", json=_request_body())

    assert resp.status_code == 200
    body = resp.json()
    assert body["result"] == '{"ok": true}'
    assert body["task_type"] == "analyze_read"
    assert body["provider"] == "gemini"
    assert body["model"] == _PRIMARY_MODEL
    assert body["usage"]["total_tokens"] == 15


def test_generate_content_auth_error_maps_to_502(client_with_provider):
    client, install = client_with_provider

    def raise_auth(_request: CompletionRequest) -> CompletionResponse:
        raise LLMAuthenticationError("bad key", provider="gemini")

    install(raise_auth)

    resp = client.post("/api/v1/generate-content", json=_request_body())

    assert resp.status_code == 502
    assert resp.json()["detail"]["code"] == "AUTHENTICATION"


def test_generate_content_invalid_request_maps_to_400(client_with_provider):
    client, install = client_with_provider

    def raise_invalid(_request: CompletionRequest) -> CompletionResponse:
        raise LLMInvalidRequestError("bad schema", provider="gemini")

    install(raise_invalid)

    resp = client.post("/api/v1/generate-content", json=_request_body())

    assert resp.status_code == 400


def test_generate_content_falls_back_to_secondary_model(client_with_provider):
    client, install = client_with_provider

    def unavailable_primary(request: CompletionRequest) -> CompletionResponse:
        # The primary model is down; the availability fallback serves the request.
        if request.model == _PRIMARY_MODEL:
            raise LLMUnavailableError("primary 503", provider="gemini")
        return CompletionResponse(
            content='{"served_by": "fallback"}',
            model=request.model,
            provider="gemini",
            usage=TokenUsage(prompt_tokens=10, completion_tokens=5, total_tokens=15),
        )

    provider = install(unavailable_primary)

    resp = client.post("/api/v1/generate-content", json=_request_body())

    assert resp.status_code == 200
    body = resp.json()
    # The response is attributed to the model that actually served it (the fallback).
    assert body["model"] == _FALLBACK_MODEL
    assert body["result"] == '{"served_by": "fallback"}'
    # Both attempts ran: primary (failed) then fallback (served).
    assert provider.calls == [_PRIMARY_MODEL, _FALLBACK_MODEL]


def test_generate_content_unknown_task_type_maps_to_400(client_with_provider):
    client, install = client_with_provider
    install(_ok("unused"))

    resp = client.post("/api/v1/generate-content", json=_request_body(task_type="does_not_exist"))

    assert resp.status_code == 400
