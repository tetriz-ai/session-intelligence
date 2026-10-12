# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Gemini system-prompt caching — a generic, opt-in reference-llm capability.

Caches the SYSTEM PROMPT only, Gemini-only, engaged per call by the request flag
`use_system_prompt_cache`. Explicit `CachedContent` create-and-reference is the primary
path (measured at a ~100% full-prompt hit rate on repeated calls); implicit caching is
incidental upside and never relied on.

The handle store is in-memory per uvicorn worker — a `{content_key -> handle}` map, no Redis.
On a worker restart the map is empty, so the first miss for a key reconciles against the
live cache objects via `caches.list` (matching the key on the cache's `display_name`) and
ADOPTS an already-live object before creating a new one — that reconcile is what stops a
restart from orphaning the previous worker's caches and paying to recreate them.

Ownership lives entirely here plus the Gemini provider hook and the usage/pricing extension;
Go stays thin (it sets the flag and reads `cache_read_tokens`).
"""

from __future__ import annotations

import asyncio
import contextvars
import hashlib
import time
from dataclasses import dataclass

import httpx
import structlog
from google.genai import types
from google.genai.errors import ServerError

from app.llm.pricing import compute_cache_storage_cost_usd

logger = structlog.get_logger(__name__)

# Request-scoped tally of the explicit-cache STORAGE cost (USD) incurred by cache CREATES during
# one request, credited here at the moment of spend. It exists because the storage cost
# otherwise rides only the creating call's CompletionResponse, and the 2.5→3.5 model fallback
# discards the primary's response when it fails over — so a cache created on the 2.5 attempt that
# then falls over would have its already-billed storage commitment dropped. Crediting at creation
# captures it regardless of which attempt's response is ultimately returned. contextvars isolate
# this per request (each handler runs in its own asyncio task); the endpoint resets it on entry
# and reads the tally when it folds cost, so it is never double-counted against the response field.
_request_cache_storage_cost_usd: contextvars.ContextVar[float] = contextvars.ContextVar(
    "request_cache_storage_cost_usd", default=0.0
)


def reset_request_cache_storage_cost() -> None:
    """Zero the per-request cache-storage tally. Call once at the start of a request handler."""
    _request_cache_storage_cost_usd.set(0.0)


def request_cache_storage_cost_usd() -> float:
    """The explicit-cache storage cost (USD) charged by every cache CREATE in this request so
    far — the total to fold into the request's billed cost, fallback-safe (see the ContextVar)."""
    return _request_cache_storage_cost_usd.get()


def credit_request_cache_storage_cost(cost_usd: float) -> None:
    """Add a cache-CREATE storage cost to the request tally. Call this from the request handler's
    OWN context (the provider, after resolve() returns) — never from inside resolve(): resolve()
    runs under asyncio.wait_for, which on Python 3.11 (the deployed runtime) executes it in a task
    with a COPIED context, so a ContextVar set there never reaches this request's tally."""
    if cost_usd > 0:
        _request_cache_storage_cost_usd.set(_request_cache_storage_cost_usd.get() + cost_usd)


def fold_cache_storage_cost_usd(request_tally_usd: float, response_field_usd: float | None) -> float:
    """The one explicit-cache storage cost to bill this request, folded from its two carriers.

    Both carry the SAME resolve()-computed charge, so the fold is a MAX, never a sum (two views of
    one charge). Each is the reliable one in a case the other misses: the request-scoped contextvar
    tally sums every create the request made (so a create on a discarded 2.5→3.5 fallback attempt is
    still billed) but is populated only when resolve() ran in the request's own context — asyncio.
    wait_for on Python <3.12 runs it in a copied context and the credit is lost there; the served
    response's cache_storage_cost_usd carries the winning attempt's cost from resolve()'s return
    value, which survives wait_for. Taking the max keeps whichever channel actually landed the cost,
    so a cached session is never under-billed. Zero on a pure cache hit.
    """
    return max(request_tally_usd, response_field_usd or 0.0)


# The reference/create call wraps a small transient-5xx retry — the POC saw one 503 clear on
# retry. Only availability-class faults are retried; a deterministic fault propagates.
_TRANSIENT_EXC = (ServerError, httpx.TransportError, httpx.TimeoutException)
_CREATE_RETRIES = 2


@dataclass(frozen=True)
class CacheHandle:
    """A live CachedContent reference: the resource name to pass as `cached_content`, and
    the instant it expires (epoch seconds) so a stale store entry is treated as a miss."""

    name: str
    expires_at: float


@dataclass(frozen=True)
class CacheResolution:
    """The outcome of resolving one call's system-prompt cache: the resource name to
    reference and whether the handle was already live (a store hit or a reconcile adopt)
    versus freshly created — surfaced so the provider can tag `cache_hit` on the trace."""

    name: str
    key: str
    hit: bool
    # One-time explicit-cache STORAGE cost (USD) this call incurred by CREATING the cache — the
    # commitment to store `cached_tokens` for the full TTL. 0.0 on a hit/adopt (which
    # creates nothing) and when the model has no storage rate. The provider surfaces it so the
    # endpoint can fold it into the call's cost_usd; per-call cached-read pricing is separate.
    storage_cost_usd: float = 0.0


def content_key(task_type: str, model: str, system_prompt: str, cache_scope: str) -> str:
    """Content-addressed cache key: `sha(cache_scope, task_type, model, sha(system_prompt))`.

    No org or version dimension on the CONTENT — the system prompt is edited in place, so an edit
    rotates the inner hash and therefore the key automatically. But a Gemini CachedContent (and its
    resource name) is scoped to the API key/project that created it, and per-tenant keys give
    different orgs different clients — so the key MUST also carry a `cache_scope` identifying that
    credential (a stable non-secret digest of the api_key), or a cache created under one key would be
    referenced from another key's client and rejected. Orgs sharing a key share the scope (and the
    cache, correctly); orgs on different keys never collide. Length-prefixed so no concatenation of
    two fields can imitate a third.
    """
    prompt_hash = hashlib.sha256(system_prompt.encode()).hexdigest()
    outer = hashlib.sha256()
    for part in (cache_scope, task_type, model, prompt_hash):
        outer.update(len(part).to_bytes(8, "big"))
        outer.update(part.encode())
    # Prefixed so the value reads as one of ours in a `caches.list` display_name.
    return f"si-sysprompt-{outer.hexdigest()[:32]}"


def _expires_at(cache: types.CachedContent, ttl_seconds: int) -> float:
    """Epoch-seconds expiry of a created/adopted cache, from its server `expire_time` when
    present, else now + the requested TTL (a floor, never later than the server would keep)."""
    expire_time = getattr(cache, "expire_time", None)
    if expire_time is not None:
        try:
            return expire_time.timestamp()
        except (AttributeError, ValueError, OSError):
            pass
    return time.time() + ttl_seconds


class PromptCache:
    """Per-worker system-prompt cache. Holds live CachedContent handles keyed by content key
    and serialises concurrent misses for the same key behind a per-key lock so two in-flight
    requests do not both create a cache for the same system prompt."""

    def __init__(self) -> None:
        self._handles: dict[str, CacheHandle] = {}
        self._locks: dict[str, asyncio.Lock] = {}

    def _lock_for(self, key: str) -> asyncio.Lock:
        lock = self._locks.get(key)
        if lock is None:
            lock = asyncio.Lock()
            self._locks[key] = lock
        return lock

    def _live_handle(self, key: str) -> CacheHandle | None:
        handle = self._handles.get(key)
        if handle is None:
            return None
        if handle.expires_at <= time.time():
            # Expired server-side — drop it so the caller re-reconciles/creates.
            self._handles.pop(key, None)
            return None
        return handle

    async def resolve(
        self,
        caches: object,
        *,
        task_type: str,
        model: str,
        system_prompt: str,
        ttl_seconds: int,
        cache_scope: str,
    ) -> CacheResolution:
        """Return the cached-content resource name to reference for this call, creating or
        adopting one as needed. `caches` is the SDK's async caches client
        (`client.aio.caches`) — any object with async `create(*, model, config)` and async
        `list()` (returning an async-iterable of CachedContent). `cache_scope` identifies the
        credential the `caches` client authenticates as, so a cache is never shared across keys
        (see content_key)."""
        key = content_key(task_type, model, system_prompt, cache_scope)

        live = self._live_handle(key)
        if live is not None:
            return CacheResolution(name=live.name, key=key, hit=True)

        async with self._lock_for(key):
            # Re-check under the lock: a racing request may have created it while we waited.
            live = self._live_handle(key)
            if live is not None:
                return CacheResolution(name=live.name, key=key, hit=True)

            adopted = await self._reconcile(caches, key)
            if adopted is not None:
                self._handles[key] = adopted
                logger.info("adopted live system-prompt cache via caches.list reconcile", cache_key=key)
                return CacheResolution(name=adopted.name, key=key, hit=True)

            created, storage_cost_usd = await self._create(
                caches, key, model=model, system_prompt=system_prompt, ttl_seconds=ttl_seconds
            )
            self._handles[key] = created
            # The create's storage cost rides back on CacheResolution.storage_cost_usd; the caller
            # (the provider, in the request handler's own context) credits the request tally, since
            # a ContextVar set HERE — inside resolve(), under asyncio.wait_for's copied context on
            # Python 3.11 — would never reach it (credit_request_cache_storage_cost's contract).
            return CacheResolution(name=created.name, key=key, hit=False, storage_cost_usd=storage_cost_usd)

    async def _reconcile(self, caches: object, key: str) -> CacheHandle | None:
        """Find an already-live cache whose display_name is this key and adopt it. Best
        effort: a `caches.list` failure is logged and treated as "nothing to adopt", so the
        caller falls through to create rather than the whole call failing on a reconcile."""
        now = time.time()
        try:
            pager = await caches.list()  # type: ignore[attr-defined]
            async for cache in pager:
                if getattr(cache, "display_name", None) != key or getattr(cache, "name", None) is None:
                    continue
                expire_time = getattr(cache, "expire_time", None)
                if expire_time is None:
                    # No server expiry to establish liveness — leave it for create to handle
                    # rather than adopt an object we cannot prove is still live.
                    continue
                try:
                    expires_at = expire_time.timestamp()
                except (AttributeError, ValueError, OSError):
                    continue
                if expires_at > now:
                    return CacheHandle(name=cache.name, expires_at=expires_at)
        except Exception as exc:  # noqa: BLE001 — reconcile is best-effort; create is the fallback
            logger.warning("caches.list reconcile failed; will create a fresh cache", cache_key=key, error=str(exc))
        return None

    async def _create(
        self, caches: object, key: str, *, model: str, system_prompt: str, ttl_seconds: int
    ) -> tuple[CacheHandle, float]:
        """Create an explicit CachedContent holding the system prompt, keyed on display_name
               so a later restart can reconcile it. Returns the handle and the one-time STORAGE cost
               (USD) of keeping it alive for its TTL, priced from the created object's own token count
        — 0.0 when the SDK reports no token count or the model has no storage rate. Wraps
               a small transient-5xx retry (the POC's single 503 cleared on retry); a deterministic fault
               propagates on the first attempt."""
        config = types.CreateCachedContentConfig(
            system_instruction=system_prompt,
            ttl=f"{ttl_seconds}s",
            display_name=key,
        )
        last_exc: Exception | None = None
        for attempt in range(_CREATE_RETRIES + 1):
            try:
                cache = await caches.create(model=model, config=config)  # type: ignore[attr-defined]
                storage_cost_usd = self._storage_cost_usd(cache, model=model, ttl_seconds=ttl_seconds)
                return CacheHandle(name=cache.name, expires_at=_expires_at(cache, ttl_seconds)), storage_cost_usd
            except _TRANSIENT_EXC as exc:
                last_exc = exc
                logger.warning(
                    "transient error creating system-prompt cache; retrying",
                    cache_key=key,
                    attempt=attempt + 1,
                    error=str(exc),
                )
        # Retries exhausted on a transient fault — raise so the provider surfaces it and the
        # cron re-picks the session, rather than silently running uncached.
        raise last_exc  # type: ignore[misc]

    @staticmethod
    def _storage_cost_usd(cache: types.CachedContent, *, model: str, ttl_seconds: int) -> float:
        """The one-time storage cost (USD) of a freshly created cache. Reads the cached token
        count off the created object's usage_metadata; getattr-guards an SDK/mock shape that omits
        it (→ 0 tokens → no cost). Never fails the create over a cost calc."""
        usage_metadata = getattr(cache, "usage_metadata", None)
        cached_tokens = getattr(usage_metadata, "total_token_count", 0) or 0
        cost = compute_cache_storage_cost_usd(model, cached_tokens, ttl_seconds)
        return cost if cost is not None else 0.0


_prompt_cache: PromptCache | None = None


def get_prompt_cache() -> PromptCache:
    """The process-wide (per-worker) PromptCache singleton, created on first use."""
    global _prompt_cache
    if _prompt_cache is None:
        _prompt_cache = PromptCache()
    return _prompt_cache
