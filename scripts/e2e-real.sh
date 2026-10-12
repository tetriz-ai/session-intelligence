#!/usr/bin/env bash
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# Real-LLM end-to-end acceptance run. Brings up Postgres + the bundled reference
# reference-llm via docker-compose, then runs the real-Gemini pipeline test against
# them. The engine runs in-process; reference-llm performs the real LLM calls.
#
# The operator supplies the provider key via the environment; this script never
# reads a key file or a repo .env:
#
#   GEMINI_API_KEY=... ./scripts/e2e-real.sh
#
# To also analyze YOUR OWN transcript through the public analyze.AnalyzeSession front door
# against the same reference-llm, pass a Conversation protojson file as the first argument:
#   GEMINI_API_KEY=... ./scripts/e2e-real.sh path/to/your-transcript.json
#
# For the fake-provider E2E (no key, no docker) see the README — that is the CI
# acceptance gate and runs with:  POSTGRES_TEST_DSN=... go test ./internal/sessionanalysis/pipelinee2e/
set -euo pipefail

if [ -z "${GEMINI_API_KEY:-}" ]; then
	echo "GEMINI_API_KEY is not set. Export it and re-run:" >&2
	echo "  GEMINI_API_KEY=... ./scripts/e2e-real.sh" >&2
	exit 2
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "==> Bringing up Postgres + reference-llm"
# Arm teardown BEFORE `up` so a failure during startup itself is still cleaned up.
trap 'docker compose down' EXIT
docker compose up -d --build --wait

# The engine (Go host) talks to the compose Postgres and the reference-llm.
export SESSIONINTEL_REAL_GEMINI_URL="http://localhost:8000"
export SESSIONINTEL_FULLSTACK_DSN="host=localhost port=5432 user=postgres password=postgres dbname=session_intelligence sslmode=disable"
export POSTGRES_TEST_DSN="$SESSIONINTEL_FULLSTACK_DSN"

echo "==> Running the real-Gemini pipeline E2E"
go test ./internal/sessionanalysis/pipelinee2e/ -run TestRealGeminiPipelineE2E -count=1 -v

# Optional: analyze YOUR OWN transcript through the public AnalyzeSession path against the same
# reference-llm. Pass a Conversation protojson file as the first argument to this script.
if [ -n "${1:-}" ]; then
	echo "==> Analyzing your transcript through analyze.AnalyzeSession: $1"
	go run ./cmd/analyze-real -url "$SESSIONINTEL_REAL_GEMINI_URL" "$1"
fi

