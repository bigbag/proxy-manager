# Development

This guide covers the contributor workflow. Read `AGENTS.md` for the repository layout and rules. Read `README.md` to run the service. Read `docs/architecture.md` for behavior contracts.

## Setup

Use Go 1.25.5 or newer. No code generation step exists. Dependencies are `github.com/redis/go-redis/v9` and `modernc.org/sqlite`.

Install `redis-server` to run the integration tests. The suite skips Redis tests when the binary is missing.

## Tests

Run a changed package first:

```sh
go test ./internal/proxy -count=1
```

Run the full suite before delivery:

```sh
make test
make test-race
```

`make test` runs `go test ./...` and then `scripts/test-release.sh`.

Tests live next to the code in `*_test.go` files. Unit tests use a temporary SQLite database, `net.Pipe`, or mocked dial functions. Integration tests spawn a real `redis-server` on a random local port with persistence disabled:

- `internal/store/redis_test.go` checks the Redis client against the spawned server.
- `internal/proxy/integration_test.go` builds the `proxy_manager` binary and starts real `run proxy` and `run api` processes.

Both skip when `redis-server` is not in `PATH`.

The helper scripts test repository tooling:

- `scripts/test-release.sh` checks `make sys/tag` input validation and `make sys/changelog` output determinism in temporary git repositories.
- `scripts/test-pre-commit.sh` checks that the pre-commit hook invokes gosec and fails when the scan fails.

## CI and hooks

GitHub Actions runs on pushes to `main` or `master` and on pull requests:

- The `test` job runs `go test -race ./...`, `scripts/test-release.sh`, and `go build ./cmd/proxy_manager`.
- The `security` job runs `make security`.

Enable the local hook with:

```sh
git config core.hooksPath .githooks
```

Each commit then runs gosec v2.29.0. Run `make security` to scan without a commit.

## Release

```sh
make sys/tag
make sys/changelog
```

`make sys/tag` asks for an `X.Y.Z` version, creates an annotated `vX.Y.Z` tag, and pushes it. `make sys/changelog` walks the `v*` tags from newest to oldest and rewrites `CHANGELOG.md`. It needs at least one tag. The changelog output is deterministic. `scripts/test-release.sh` checks this property.

## Add a provider

1. Create `internal/provider/<name>.go`. Implement the `Fetcher` interface from `internal/provider/refresh.go`: `Name()`, `Interval()`, and `Fetch(ctx)`.
2. Add a `case` in the `fetchers` function in `cmd/proxy_manager/main.go`. Set the default endpoint and interval there.
3. Add any new JSON fields to `ProviderSettings` in `internal/config/config.go`. The loader rejects unknown fields, so a missing field stops startup with a clear error.
4. Keep the provider name free of `_` and `:`. The proxy ID format `provider_host_port` and the Redis key format use these characters as separators.
5. Add the name to a route in `routes.json`. A route that names an unfetched provider returns `502` to clients.
6. Add a fetch test in `internal/provider/provider_test.go`. Test pagination, invalid records, and credential redaction in error output.

## Add a routing strategy

1. Add a constant to the `Strategy` type in `internal/config/config.go`.
2. Add a `case` to the strategy switch in `internal/pool/pool.go`. The `default` branch selects no proxy.
3. Accept the new value in the route validation in `internal/config/config.go`.
4. Update `docs/architecture.md` with the selection semantics.

## Change shared state

Keep new shared state in the Redis client and new local state in the SQLite client. Do not add a third store. When you change a Redis key name, an error string, or an environment variable name, update `docs/architecture.md` in the same change.

The Redis client migrates the legacy Python latency format on first read. Keep `migrateLatencyScript` in `internal/store/redis.go` working when you touch latency storage.

## Load checks

`cmd/loadprobe` checks CONNECT capacity against an isolated local Redis database with a mocked upstream. It refuses non-empty, remote, or non-localhost Redis URLs.

```sh
make load LOAD_FLAGS='-concurrency 1000 -duration 30s'
```

Set `REDIS_TEST_URL` to a scratch database such as `redis://127.0.0.1:6379/15`. The tool prints attempts, errors, throughput, and RSS/FD/goroutine baselines.
