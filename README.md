# Proxy manager

This Go service accepts HTTP `CONNECT` requests and selects an upstream HTTP proxy. It stores proxy lists, affinity, health, and statistics in Redis or SQLite.

## Build and run

Use Go 1.25.5. Run `make test`, `make vet`, `make lint`, and `make build` before deployment.

Start one refresher for each Redis instance. Start one or more proxy nodes against the same Redis instance. Start the read-only API separately. Use SQLite with one node and a local database file. Do not share a SQLite file between proxy nodes.

```sh
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 make refresh/once
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 make refresh
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 make run/proxy
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 make run/api
go run ./cmd/proxy_manager stats
go run ./cmd/proxy_manager list-proxies
```

Set `STORE=sqlite` and `SQLITE_PATH=/var/lib/proxy-manager/proxy.db` for a local store. The API and CONNECT server have no client authentication. Both listeners bind to `127.0.0.1` by default. Set `PROXY_HOST` or `API_HOST` to a reachable address only behind trusted-client network controls.

The CONNECT listener uses plain TCP. The client-to-service request exposes the target host and request headers on that link. The separate service-to-provider request exposes upstream Basic credentials when the provider proxy uses them. HTTPS in the accepted tunnel does not encrypt either CONNECT request. Keep both links on trusted or encrypted networks. See [architecture](docs/architecture.md) for the two connection paths.

Keep the SQLite directory writable only by the service account. The service restricts the database and its WAL and SHM files to owner access when it opens them.

Send `CONNECT example.com:443 HTTP/1.1` to the proxy. Add `X-Proxy-Affinity: session-key` to use the same healthy upstream on later requests. The service does not send this header to the upstream. `PROXY_AFFINITY_TTL=0` disables affinity.

To check sticky selection, start the proxy with `LOG_LEVEL=INFO STORE=redis make run/proxy`. Keep `PROXY_AFFINITY_TTL` above zero. Run these requests in another terminal:

```sh
for i in 1 2 3; do
  curl -fsS --max-time 20 --noproxy '' -x 'http://127.0.0.1:8080' \
    --proxy-header 'X-Proxy-Affinity: sticky-check-1' \
    'https://api.ipify.org?format=json'
  printf '\n'
done
```

Check the `upstream selected` log entry for each request. The same `proxy_id` shows that the bind works. The service writes the bind after an upstream `200` response. A matching public IP alone does not prove that the same proxy handled each request. Use `--proxy-header`, not `-H`, to send the key in CONNECT.

## Releases

Commit the release changes. Run `make sys/tag` and enter an `X.Y.Z` version. The target creates an annotated `vX.Y.Z` tag and pushes it to `origin`. Run `make sys/changelog` to generate `CHANGELOG.md` from version tags. Commit the generated file after the release. The changelog target stops without changing the file when no version tags exist.

## Configuration

The service reads the process environment and a provider JSON file. It reads `ROUTES_CONFIG_PATH` at service startup. If this file is absent, it uses one `*` route with `webshare`, `proxyrack`, and `rayobyte` in `priority_fallback` order.

The Makefile loads `.local_env` for `run/proxy`, `run/api`, `refresh`, and `refresh/once` when the file exists. Set `LOCAL_ENV_FILE` to use another file. Direct `go run` commands read only exported process variables and the provider JSON file. Quote shell syntax in `.local_env` values.

- `STORE` selects `redis` by default. Set it to `sqlite` for a local database.
- `REDIS_URL` sets the Redis connection URL. It defaults to `redis://127.0.0.1:6379/0`.
- `SQLITE_PATH` sets the local database file. It defaults to `proxy.db`.
- `ROUTES_CONFIG_PATH` sets the route file. It defaults to `routes.json`.
- `LOG_LEVEL` sets the minimum log level. It defaults to `INFO`. Use `DEBUG`, `INFO`, `WARN`, or `ERROR`.
- `PROXY_HOST` and `PROXY_PORT` set the CONNECT listener. They default to `127.0.0.1` and `8080`.
- `PROXY_CONNECT_TIMEOUT` limits the handshake in seconds. It defaults to `30`.
- `PROXY_BUFFER_SIZE` sets the tunnel copy buffer in bytes. It defaults to `16384`.
- `PROXY_AFFINITY_TTL` sets the bind lifetime in seconds. It defaults to `1800`. Set it to `0` to disable affinity.
- `PROXY_MAX_CONNECTIONS` sets the active connection limit per node. It defaults to `1024`.
- `API_HOST` and `API_PORT` set the API listener. They default to `127.0.0.1` and `8081`.
- `HEALTH_FAILURE_THRESHOLD` sets the number of consecutive upstream failures before a health mark. It defaults to `3`.
- `HEALTH_COOLDOWN_SECONDS` sets the health mark lifetime. It defaults to `300`.
- `PROVIDERS_CONFIG_PATH` selects the provider JSON file. It defaults to `providers.json`. A missing default file enables no providers. An explicit missing file stops startup.

Use `rediss://` or an encrypted link when Redis traffic crosses an untrusted network. A plain `redis://` connection sends stored proxy credentials without transport encryption.

Copy `providers.json.example` to `providers.json`. Set its permissions to `0600`. Add one top-level entry per enabled provider. Remove entries for unused providers. Set credentials before you run refresh. Keep `providers.json` private; Git ignores this file. Each entry accepts these fields:

- `api_key` sets the API key for Webshare, ProxyRack, or Rayobyte.
- `api_login` sets the API login for ProxyRack or Rayobyte.
- `base_url` sets the Webshare or Rayobyte endpoint. The defaults are `https://proxy.webshare.io/api/v2/proxy` and `https://rayobyte.com/proxy/dashboard/api`.
- `address` sets the ProxyRack host. It defaults to `proxy.proxyrack.net`.
- `initial_port` sets the first ProxyRack port. It defaults to `10000`.
- `slots` sets the ProxyRack port count. It defaults to `100`.
- `refresh_interval_minutes` sets the refresh interval. It defaults to `30` for Webshare and Rayobyte, or `60` for ProxyRack.

Use HTTPS for provider endpoints. An HTTP override or an HTTPS-to-HTTP redirect can expose an API key. The Rayobyte API puts its login and key in the request path, which the provider can retain in access logs. Limit access to those logs.

The service enables an entry when it finds its provider name in the file. Do not use `_` or `:` in provider names. It stops startup for invalid JSON or unknown fields. Refresh rejects a provider name without a fetcher. To add a new provider type, implement a fetcher and add its name to the `fetchers` switch. Add that name to `routes.json` to route CONNECT requests to it.

For an existing deployment, move provider credentials and settings from the old environment entries into the JSON file. Remove the old entries. Set `PROVIDERS_CONFIG_PATH` to an absolute path when the working directory can change. Restart the proxy, API, and refresher after the change.

The service writes time, level, component, and event data to standard error. `LOG_LEVEL=WARNING` uses `WARN`. `LOG_LEVEL=CRITICAL` or `LOG_LEVEL=FATAL` uses `ERROR`. `LOG_LEVEL=NOTSET` uses `DEBUG`.

Example `routes.json`:

```json
{"routes":[{"pattern":"*.example.com","providers":["webshare","rayobyte"],"strategy":"round_robin"},{"pattern":"*","providers":["proxyrack"],"strategy":"priority_fallback"}]}
```

The first matching pattern wins. Patterns use case-sensitive glob matching. A route with no available proxy returns `502`.

## Statistics and load checks

The API serves `GET /`, `/health`, `/stats`, `/stats/{proxy_id}`, `/providers`, `/routes`, and `/metrics`. `GET /` lists the other API paths. `/health` returns `503` if the store is unavailable. `/metrics` exposes provider counts, durable request and byte counters, and CONNECT handshake duration sums and counts for Prometheus. The API and CLI do not print upstream passwords. See [architecture](docs/architecture.md) for API fields and the provider mean duration query.

Use a new local Redis database for each probe. The probe refuses remote or nonempty Redis databases. These examples exercise the mock upstream, not a provider account:

```sh
make load REDIS_TEST_URL=redis://127.0.0.1:6379/15
make load REDIS_TEST_URL=redis://127.0.0.1:6379/14 LOAD_FLAGS='-concurrency 1000 -duration 30s -hold'
```

The load probe reads `REDIS_TEST_URL` from its environment. It does not accept a Redis URL flag. The Make target does not print the URL. For an authenticated local Redis, load the variable from a protected environment file. Do not put an authenticated URL in a Make command: shell history and process arguments can retain it.

The probe prints attempts, successes, errors, average CONNECT latency, process RSS, file descriptors, goroutines, and Redis command count. Resource columns show baseline, observed peak, and post-shutdown values. The close mode can receive `503` when requests exceed the per-node active connection limit. A high attempt rate is not a successful CONNECT rate. The hold mode measures active tunnels, not sustained request throughput.

Local probe results on 2026-09-25: Linux 7.1.9 x86_64, eight logical CPUs, 62 GiB RAM, Go 1.27.0 development toolchain, Redis 8.10.1, and an open-file limit of 500000. The probe uses a local mock upstream and a new Redis database per run. The node limit equals the requested concurrency. RSS shows process memory in decimal MB. Each resource column shows baseline / observed peak / after tunnel shutdown.

- Close mode with 1000 clients:
  - The run lasts 3.06 seconds. It records 75311 attempts, 19590 successes, and 55721 errors.
  - The attempt rate is 24648 per second. The mean CONNECT latency is 109.6 ms.
  - RSS is 14.6 / 84.6 / 74.1 MB. Open files are 16 / 2759 / 94.
  - Goroutines are 4 / 2126 / 2. Redis processes 235397 commands.
- Hold mode with 1000 clients:
  - The run lasts 3.01 seconds. It records 1000 attempts and 1000 successes.
  - The attempt rate is 332 per second. The mean CONNECT latency is 68.6 ms.
  - RSS is 14.6 / 117.1 / 119.3 MB. Open files are 16 / 4095 / 94.
  - Goroutines are 4 / 4005 / 2. Redis processes 12439 commands.
- Hold mode with 10000 clients:
  - The run lasts 5.09 seconds. It records 10000 attempts and 10000 successes.
  - The attempt rate is 1965 per second. The mean CONNECT latency is 664.2 ms.
  - RSS is 14.6 / 988.9 / 991.3 MB. Open files are 16 / 40095 / 94.
  - Goroutines are 3 / 40005 / 2. Redis processes 121425 commands.

The close run samples `503` responses. Each client starts another CONNECT before the node finishes the prior tunnel and statistics update. This reaches the configured active connection limit. The hold runs establish 1000 and 10000 tunnels without a CONNECT error on this workstation. They do not establish a general capacity guarantee. After shutdown, open files and goroutines decrease. RSS stays high because the Go process retains allocated memory until exit.

## Security scan

Enable the local Git hook with `git config core.hooksPath .githooks`. Each commit runs `go run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./...`. Run `make security` to scan without a commit. GitHub Actions runs race tests, a build, and the same scan on pushes and pull requests.

See [docs/architecture.md](docs/architecture.md) for services, routing, health, storage, and API contracts.
