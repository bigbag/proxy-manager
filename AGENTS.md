# proxy-manager

This file applies to the whole repository. A user instruction overrides this file.

The Go 1.25.5 service accepts HTTP `CONNECT` requests. It selects an upstream proxy. It stores state in Redis or SQLite. Read `docs/architecture.md` for product behavior. Read `README.md` for configuration and deployment.

## Commands

```sh
make help
make test
make test-race
make vet
make lint
make build
make security
make run/proxy
make run/api
make refresh
make refresh/once
```

Run a changed package first. For example, run `go test ./internal/proxy -count=1`. Run the full suite before delivery. Do not start `make run/proxy`, `make run/api`, or `make refresh` unless the user asks. These commands run until the process stops. Use `make refresh/once` to fetch lists one time.

The Makefile loads `.local_env` for `run/proxy`, `run/api`, `refresh`, and `refresh/once` when the file exists. Use `LOCAL_ENV_FILE` to select another file. Direct Go commands read only exported process variables. Do not print values from `.local_env`.

## Layout

- `cmd/proxy_manager` parses commands and handles process shutdown.
- `cmd/loadprobe` checks CONNECT load against an isolated local Redis database.
- `internal/config` reads environment settings and route rules.
- `internal/store` stores proxies, health, affinity, and statistics in Redis or SQLite.
- `internal/route` matches a host to providers.
- `internal/pool` selects an upstream proxy.
- `internal/proxy` handles CONNECT and copies tunnel data.
- `internal/provider` fetches provider lists.
- `internal/api` serves the read-only statistics API.

## Code and writing rules

Follow KISS/DRY principles. Keep solutions simple.
If code clearly describes what it does, do not add comments.
Match existing style, even if you would do it differently.
Write all documentation, comments, and docstrings in Simplified Technical English (ASD-STE100). Use one idea per sentence. Use active voice and present tense. Use approved words in their approved meaning. Do not use filler or cliches.

Use `gofmt` for Go changes. Put tests next to the code they check. Use the Go `testing` package. Do not add a new internal package for one helper.
Use the configured `LOG_LEVEL` for service logs. Log startup, CONNECT, refresh, and health events. Do not log credentials.

## Product and security rules

- Keep `CONNECT` as the only proxy method. Do not add client authentication or HTTP method forwarding.
- Copy a valid upstream HTTP error status to the client. Do not replace it with `502`.
- Do not log passwords or full credential-bearing URLs. Keep credentials out of API and CLI output.
- Keep new shared state in the Redis client. Keep new local state in the SQLite client. Do not add a third store.
- Change Redis key names, error strings, or environment names only when you also update `docs/architecture.md`.
- Run one refresher for each shared Redis instance. Use SQLite with one node and a local file.
