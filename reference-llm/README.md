# reference-llm

The open-source **reference LLM service** behind the session-intelligence engine. It is a
small FastAPI app that turns the engine's requests into Google Gemini calls (prompt routing,
structured JSON output, model fallback, cost accounting) and exposes one placeholder ML
endpoint. The Go engine calls it over plain HTTP.

## What it does

- Routes a `task_type` to a system prompt and model config, calls Gemini, and returns the
  structured result plus token usage and an estimated USD cost.
- Falls back from a primary Gemini model to a secondary one on an availability failure.
- Optionally records traces to a self-hosted Langfuse (disabled by default; raw transcripts
  are never exported unless explicitly enabled).

## Endpoints

- `POST /api/v1/generate-content` — the main endpoint. Runs an LLM task (the session-analysis
  producers `analyze_read` / `analyze_evidence` / `analyze_discard`, plus `analyze_prompt` and
  `analyze_loops`) and returns the model's JSON, the model/provider used, token usage, and cost.
- `POST /api/v1/detect-round-trips` — scores prompt/response/prompt triples with a local
  MiniLM + NLI detector. **The shipped classifier is an uncalibrated placeholder**, so the
  detector is **disabled by default** and this endpoint returns `503` until an operator bakes a
  real model bundle and enables it (see `ML_MODEL_INFERENCE__DETECTOR_ENABLED`). Running it needs
  the optional `ml` dependency extra (`uv sync --extra ml`).
- `GET /_health`, `GET /readyz` — liveness / readiness. `GET /metrics` — Prometheus metrics.

## Running it

Prerequisites: [uv](https://docs.astral.sh/uv/) and a Google Gemini API key.

```bash
# Install dependencies (base + dev tooling; the heavy ML stack is NOT installed).
uv sync

# Provide your Gemini key and start the service. The single LLM__GEMINI_API_KEY env var
# configures the provider in every environment — bring your own key and it runs.
LLM__GEMINI_API_KEY="your-gemini-key" ENVIRONMENT=development \
  uv run uvicorn app.main:app --host 0.0.0.0 --port 8000

# Or use the module entrypoint, which honors SERVER__HOST / SERVER__PORT / SERVER__WORKERS:
LLM__GEMINI_API_KEY="your-gemini-key" ENVIRONMENT=development uv run python -m app.main
```

The service boots without a key (health checks pass), but LLM calls then fail until
`LLM__GEMINI_API_KEY` is set.

### Docker

```bash
docker build -t reference-llm .
docker run -p 127.0.0.1:8000:8000 -e LLM__GEMINI_API_KEY="your-gemini-key" reference-llm
```

The image defaults to `ENVIRONMENT=prod`, which hides the `/docs` Swagger UI. The port is
published on loopback only (matching `docker-compose.yml`) because the service is
unauthenticated — see [Operational trust boundary](#operational-trust-boundary).

## Configuration

Config is layered: shared defaults in `app/config/values/tetriz-py-params.yml`, then the
environment file `tetriz-py-<ENVIRONMENT>.yml` (e.g. `-development.yml`), then environment
variables (highest priority). Nested keys use `__`, e.g. `SERVER__PORT`.

| Variable | Purpose | Default |
| --- | --- | --- |
| `ENVIRONMENT` | `development` \| `deploy` \| `prod`. Non-development hides `/docs`. | (required) |
| `LLM__GEMINI_API_KEY` | The Gemini provider key. | empty (calls fail) |
| `LLM__REQUEST_TIMEOUT` | Per-call Gemini HTTP timeout (seconds). | `180` |
| `LLM__GEMINI_PRIMARY_MODEL` / `LLM__GEMINI_FALLBACK_MODEL` | Model fallback pair. | `gemini-2.5-flash` / `gemini-3.5-flash` |
| `FEATURES__GEMINI_MODEL_FALLBACK_ENABLED` | Enable the primary→fallback model swap. | `false` |
| `FEATURES__ANALYZE_PROMPT_VALIDATION` | Run the analyze_prompt output validators. | `false` |
| `SERVER__HOST` / `SERVER__PORT` / `SERVER__WORKERS` / `SERVER__LOG_LEVEL` | Listen settings. | `0.0.0.0` / `8000` / `1` / `info` |
| `LANGFUSE__ENABLED` | Export traces to Langfuse. | `false` |
| `LANGFUSE__SEND_RAW_TRANSCRIPTS` | Send raw transcripts to traces (session-analysis producers stay redacted regardless). | `false` |
| `ML_MODEL_INFERENCE__DETECTOR_ENABLED` | Load the round-trip detector (needs a real bundle + the `ml` extra). | `false` |

## Development

```bash
uv run ruff check .          # lint
uv run ruff format --check . # formatting
uv run pytest                # tests (hermetic: no network, no real key)
```

## Operational trust boundary

This service is **unauthenticated**. Its `/api/v1/generate-content` and `/api/v1/detect-round-trips`
endpoints have no per-request auth or rate limiting, and it holds a single shared
`LLM__GEMINI_API_KEY` that backs every caller. Run it only on a trusted private network reachable by
your own backend — never expose it to the public internet, where any client could drive unbounded
provider spend or submit arbitrary content. A coarse request-body-size cap is the only built-in
request guard.

Tracing (Langfuse) is **disabled by default**, and when enabled the session-analysis producers'
transcript *inputs* are withheld from spans. Model *output* prose is not scrubbed before export, so
if you enable tracing, treat your Langfuse project as a destination that may receive model-emitted
text and scope its access accordingly.
