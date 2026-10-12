# Reference LLM service for the session-intelligence engine.
# Exposes POST /api/v1/generate-content (session-analysis + analyze-prompt task
# types) and POST /api/v1/detect-round-trips. The provider key is supplied at run
# time via the LLM__GEMINI_API_KEY environment variable and is never baked in.
FROM python:3.11-slim

WORKDIR /app

# Pin uv for reproducible dependency resolution.
ARG UV_VERSION=0.10.4
RUN pip install --no-cache-dir "uv==${UV_VERSION}"

# Resolve the locked dependency set first for layer caching. --no-install-project
# installs only third-party deps (the project itself is installed after the source is
# copied). --no-dev and no --extra means the heavy ML stack (torch / sentence-transformers,
# the `ml` extra) is intentionally excluded — the detector ships disabled.
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project

COPY app ./app
# Install the project itself on top of the cached dependency layer.
RUN uv sync --frozen --no-dev --no-editable

ENV PYTHONUNBUFFERED=1
# Make the project's virtualenv the default interpreter path.
ENV PATH="/app/.venv/bin:$PATH"
# Default runtime environment. "prod" (the production value of the Environment enum) hides
# /docs (Swagger) and loads the shared params over the built-in defaults. A host targeting
# development/deploy overrides ENVIRONMENT at run time. The key still arrives via LLM__GEMINI_API_KEY.
ENV ENVIRONMENT=prod
# The PAIRS round-trip detector ships as a placeholder classifier only; the real
# corpus-fitted bundle is not distributed, so the detector is disabled by default.
# A host that bakes a real bundle into /app/artifacts (and installs the `ml` extra)
# flips this to true.
ENV ML_MODEL_INFERENCE__DETECTOR_ENABLED=false

# Run as a non-root user.
RUN useradd --system --create-home --uid 10001 appuser && chown -R appuser:appuser /app
USER appuser

EXPOSE 8000
# Entrypoint honors SERVER__HOST / SERVER__PORT / SERVER__WORKERS / SERVER__LOG_LEVEL
# via the config layer (see app.main:main).
CMD ["python", "-m", "app.main"]
