# Contributing

## Build, test and lint

The toolchain floor is Go 1.25.0. From a fresh clone:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...          # DAO + E2E tests skip without POSTGRES_TEST_DSN
scripts/run-examples.sh               # every example runs and its output is compared
```

golangci-lint v2.11.4 is the pinned version CI runs. It panics under Go 1.26+, so run it under
Go 1.25:

```sh
GOTOOLCHAIN=go1.25.9 golangci-lint run
```

CI tests on Go 1.25.0, 1.26.x and 1.27.x; the lint job runs on 1.25.x for the reason above.

The DAO and pipeline end-to-end tests need Postgres. Point them at one and load the schema:

```sh
createdb session_intelligence_test
psql -d session_intelligence_test -f migrations/latest.sql
export POSTGRES_TEST_DSN="host=localhost port=5432 dbname=session_intelligence_test sslmode=disable"
```

`POSTGRES_TEST_DSN` also accepts a URL. To reuse the Postgres that `docker compose up` starts
(service `postgres`, user/password `postgres`, database `session_intelligence`), set:

```sh
export POSTGRES_TEST_DSN="postgres://postgres:postgres@localhost:5432/session_intelligence"
```

## Pull requests

- **Every pull request needs a test.** A behaviour change needs a test that fails before the
  change and passes after; new DAO or engine cases go beside the code.
- **Keep commits focused**, with messages in the imperative mood.
- **Add a `CHANGELOG.md` entry for any user-visible change.** A pure refactor or test needs none.
- **Do not silence a lint finding inline.** Fix it, or change `.golangci.yml` with a comment
  explaining why.
- **New public-API demonstrations go under `examples/`** and are linked from
  [`examples/README.md`](examples/README.md) as an index row. CI runs every example through
  `scripts/run-examples.sh`, which diffs each example's output against its README, so keep that
  output deterministic (fixed timestamps, sorted output).

## Review

A maintainer reviews pull requests.

## Licensing of contributions

No sign-off line and no separately signed agreement are needed: what you submit in a pull request
is offered under the same license as the rest of the project (the [LICENSE](LICENSE)).
