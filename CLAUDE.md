# Agent guide

## What this is

A Go engine that turns a coding-agent session transcript into a versioned analysis: turns,
phases, intent, evidence, outcomes, quality signals, round trips and security flags. It is
single-tenant and persists to Postgres; multi-tenancy, seat scoping and the dashboard-serving
reads are not part of this repo. The Go module path is `github.com/tetriz-ai/session-intelligence`.

Two entry points: `analyze.AnalyzeSession` is the public single-session front door — one
transcript in, the `Analysis` back, no database or config; the internal batch path
(`internal/sessionanalysis`, `ProcessFlat`) drains unprocessed records from Postgres.

## Layout

- `analyze/` — the public API: `AnalyzeSession`, the content-generation `Provider` and `RoundTripDetector` seams, `ParseConversation`, and the clean `Analysis` result. Internal-free, so outsiders import only this.
- `security/` — the security detector's flag reconcile (`ReconcileOpenFlags`); tests beside it.
- `dao/model/` — the public flag and record models the API surfaces. (`analyze/`, `security/`, `dao/model/` and `gen/` are the public import surface; everything under `internal/` may change without notice.)
- `internal/sessionanalysis/` — the engine: spine, R/E/D producers, commit, round-trip detection, and the single-session core shared with `analyze/`. Tests live beside the code.
- `internal/dao/` — Postgres repositories and reference readers for input tables.
- `internal/` (rest) — vendored/shimmed support (logger, pagination, storage, LLM client, tenancy no-ops).
- `gen/`, `proto/` — generated protobuf Go and the `.proto` sources. The exported API also renders at https://pkg.go.dev/github.com/tetriz-ai/session-intelligence once published.
- `reference-llm/` — the bundled reference LLM service (FastAPI).
- `migrations/latest.sql` — the session/security schema. `examples/` — runnable offline examples. `cmd/analyze-real/` — the real-LLM CLI over `AnalyzeSession`. `scripts/` — the examples and E2E runners.

## Commands

Toolchain floor is Go 1.25.0. CI tests on 1.25.0, 1.26.x and 1.27.x. Lint is golangci-lint v2.11.4, which must run under Go 1.25 because it panics on Go 1.26+.

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...              # DAO + pipeline tests skip without POSTGRES_TEST_DSN
scripts/run-examples.sh                   # runs every example, compares output to its README
GOTOOLCHAIN=go1.25.9 golangci-lint run    # v2.11.4; pin Go 1.25, the linter panics on 1.26+
```

DAO and pipeline tests need Postgres; set it up as CONTRIBUTING.md shows and load `migrations/latest.sql`.

## Contract to keep

- **A detected security-layer issue always becomes an open flag, never silently dropped.** An unspecified severity is published at "medium". Pinned by `security/reconcile_test.go` (`TestReconcileUnspecifiedSeverityDefaultsToMedium`) and `dao/security_flag_repo_test.go`.
- **Excerpts are masked as a backstop, not sanitized here.** Redacting a finding's raw value is the producer's job; this layer masks only well-known shapes. Pinned by `security/reconcile_test.go` (`TestReconcileMasksKnownSecretShapesInExcerpt`).
- **Reconcile maps slug→flag_type and promotes to the highest severity per type.** Pinned by `security/reconcile_test.go`.
- **The pipeline persists session_analyses (+ turn/phase/state) and security_flags end-to-end.** Pinned by `internal/sessionanalysis/pipelinee2e/` (fake-provider E2E).
- **`ProcessFlat` returns `(count, error)` and never panics on a missing/failed producer;** the LLM seam is the `LLMProducer` interface with a fake in tests. Pinned by `internal/sessionanalysis/sessionanalysis_test.go`.
- **`analyze.AnalyzeSession` runs one conversation in memory with no DB or config and returns the `Analysis`;** its provider seam is content-generation (`Provider`), driven by a fake in tests. Pinned by `analyze/analyze_test.go`.
- **`analyze.ParseConversation` reads a `local-session-extractor` record as well as the Conversation protojson,** converted to the `Conversation` shape: the source maps to `IDE_TYPE_*`, and every message with its metadata, `session_meta`, the session events and their coverage are kept. Pinned by `analyze/agentdesk_test.go` against the extractor's golden export, copied to `analyze/testdata/agentdesk/` (refresh it from the extractor's `fixtures/golden.jsonl` when its record format changes).
- **Single-tenant:** no seat/data-window gating; all persons and records are processed. Pinned by the DAO tests loading `migrations/latest.sql` (no RLS).

## Making a change

- Follow [CONTRIBUTING.md](CONTRIBUTING.md) for the contributor and PR rules: every behaviour change needs a test (failing before, passing after), user-visible changes get a `CHANGELOG.md` entry, lint findings are fixed rather than silenced inline, and a maintainer reviews.
- New DAO/engine cases go beside the code; new public-API demonstrations go under `examples/` and are linked from `examples/README.md` as an index-row `| you want ... | [name](name/) |`.
- CI runs the examples and the README blocks. The runner extracts each `examples/<name>/README.md`'s first ` ```sh ` block, runs it from the repo root, and diffs stdout against the last ` ```text ` block. Keep example output deterministic (fixed timestamps, sorted output) so the comparison holds.

## Do not

- Do not add seat/tenant scoping or serving-aggregate reads; this engine is single-tenant and they are out of scope.
- Do not hand-edit the `gen/` stubs; regenerate from `proto/` with `buf generate` (run `buf dep update` first after changing `buf.yaml` deps). Config is `buf.yaml` + `buf.gen.yaml`.
- Do not lower the Go floor below 1.25.0 (a dependency requires it).
- The `ghp_`/token strings in `internal/logredact` tests are fixtures, not secrets.
- Do not read a real credential store or call a live LLM in tests; the suite uses fakes.
