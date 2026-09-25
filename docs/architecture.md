# Architecture

## Services and configuration

The `proxy_manager` binary provides these commands:

- `make run/proxy` runs `run proxy`. It accepts CONNECT requests and copies tunnel data.
- `make run/api` runs `run api`. It serves the read-only HTTP API and Prometheus metrics.
- `make refresh` runs `refresh`. It fetches enabled provider lists at startup. It checks each refresh interval every minute.
- `make refresh/once` runs `refresh --once`. It fetches enabled provider lists one time. It prints provider counts as JSON.
- `stats` prints the same records as `GET /stats` as JSON.
- `list-proxies` prints current proxy IDs, providers, hosts, and ports as JSON.

The proxy, API, and continuous refresher run until a signal stops them. Start one refresher per shared Redis instance. Multiple proxy nodes can share that Redis instance. Use SQLite with one node and one local file. The API and proxy processes can open the same local SQLite file on that node. The service checks the store connection before it starts a listener.

The service reads `STORE`, `REDIS_URL`, `SQLITE_PATH`, `ROUTES_CONFIG_PATH`, `PROVIDERS_CONFIG_PATH`, `LOG_LEVEL`, `PROXY_HOST`, `PROXY_PORT`, `PROXY_CONNECT_TIMEOUT`, `PROXY_BUFFER_SIZE`, `PROXY_AFFINITY_TTL`, `PROXY_MAX_CONNECTIONS`, `API_HOST`, `API_PORT`, `HEALTH_FAILURE_THRESHOLD`, and `HEALTH_COOLDOWN_SECONDS`. The defaults appear in [README.md](../README.md).

The Makefile loads `.local_env` for the proxy, API, and refresh targets when the file exists. `LOCAL_ENV_FILE` selects another file. Direct binary commands read the process environment and the provider JSON file.

`LOG_LEVEL` controls startup, CONNECT, refresh, and health logs. The service writes the time, level, component, and event to standard error. At `INFO`, the service logs the selected `proxy_id` with the CONNECT host and port. Logs omit credentials and full Redis URLs.

The service reads route rules at startup. It matches the first case-sensitive glob pattern. It uses `round_robin` when a route omits a strategy. If `routes.json` is absent, it uses one `*` route with `webshare`, `proxyrack`, and `rayobyte` in `priority_fallback` order. An invalid existing route file stops startup. A route can use a disabled provider, but that provider has no refreshed proxies.

## Provider lists

The service reads a provider map from `PROVIDERS_CONFIG_PATH`. It uses `providers.json` by default. A missing default file enables no providers. A missing explicit file or invalid JSON stops startup. Each top-level key names one enabled provider. Proxy IDs use `_` as a separator. Redis keys use `:` as a separator. Provider names cannot contain these characters. The file holds provider credentials and stays outside version control. Add a provider entry to enable a built-in fetcher. Add a fetcher and a case in `fetchers` for a new provider type. Add the provider name to a route to select it for CONNECT.

The refresher fetches all enabled providers at startup. It checks each configured refresh interval once per minute. A failed fetch keeps that provider's old list. A successful empty fetch clears that list. An error produces a zero count in `refresh --once` output and an error log.

- Webshare reads direct proxy JSON pages of 100 records. It refreshes every 30 minutes by default. It skips invalid and duplicate records. It waits at least three seconds between pages.
- ProxyRack uses the configured address and consecutive ports. It refreshes every 60 minutes by default. It checks the first port before it creates the list. It keeps the old list if the check fails.
- Rayobyte reads `host:port:user:password` CSV records. It refreshes every 30 minutes by default. It skips invalid records.

A proxy ID uses `<provider>_<host>_<port>`. A record holds provider, host, port, user, and password. The service sends Basic authentication to an upstream only when both user and password exist. Logs, the API, and `list-proxies` omit passwords.

The Rayobyte export API puts the login and key in the request path. The default endpoint uses HTTPS, but provider access logs can keep the path. Restrict access to these logs. Do not log the full request URL.

## Routing patterns, selection, and affinity

The proxy matches the CONNECT destination host against route patterns in file order. It uses the first matching route. It matches host names case-sensitively. The pattern `*.example.com` matches `api.example.com`, but it does not match `example.com`. Add a final `*` route to handle other hosts. The API reports its route snapshot through `GET /routes`. Restart each proxy or API process after a route file change.

The service selects only proxies in the matched route. It sorts healthy proxies by ID. `round_robin` uses one atomic counter for the route provider set. `priority_fallback` selects the first provider with a healthy proxy and uses that provider's counter. If no proxy is healthy, a request without affinity selects a random proxy from the route. An empty route list returns `502 Bad Gateway`.

The client can send `X-Proxy-Affinity` with at most 256 bytes. An empty key or `PROXY_AFFINITY_TTL=0` disables affinity. A valid bind uses a proxy that exists, belongs to the route, and is healthy. A missing or invalid bind selects the first healthy proxy by route provider order and proxy ID. If none is healthy, it selects the first available proxy by that order. The service stores or refreshes a bind only after the upstream accepts CONNECT and the client receives `200`. Concurrent bind writes use the last successful write.

When affinity is enabled, the service retries once with a different available proxy after a failure before a valid upstream HTTP response. It does not retry without affinity, after an upstream HTTP response, or after it sends the client `200`. A missing alternative or a second failure returns `502`. An upstream non-200 HTTP status goes to the client without a retry.

## CONNECT and health

The CONNECT server binds to `127.0.0.1` by default. It has no client authentication. Use an external bind only behind trusted-client network controls. It accepts one CONNECT request per client socket. It applies `PROXY_CONNECT_TIMEOUT` to the complete CONNECT handshake. It limits each client or upstream response head to 64 KiB.

The client sends CONNECT to the service over plain TCP. An observer on this leg can read the target host and request headers. The service makes a separate plain TCP connection to the upstream provider proxy. It sends Basic proxy credentials on that leg when the proxy has a user name and password. Base64 does not encrypt the credentials. HTTPS inside an accepted tunnel protects the payload, but it does not protect either CONNECT request. An internal client-to-service link does not protect the service-to-provider link. Keep both links on trusted or encrypted networks. Use a TLS-capable upstream proxy or an encrypted tunnel when the provider supports one.

- `200 Connection Established` starts a tunnel after the upstream accepts CONNECT.
- `400 Bad Request` rejects an invalid request or port. It also rejects a head over 64 KiB or an affinity key over 256 bytes.
- `405 Method Not Allowed` rejects a method other than CONNECT.
- `502 Bad Gateway` reports that no proxy is available or that the upstream sends no valid HTTP response.
- `503 Service Unavailable` reports that the node reaches `PROXY_MAX_CONNECTIONS`.
- A valid non-200 upstream status goes to the client without a retry.

For an upstream non-200 response, the service copies the HTTP status and at most 1 MiB of decoded body data. It removes the upstream body-length framing and closes the client response.

The service starts a tunnel only after it receives an upstream `200`. It copies data in both directions with half-close support. It has no tunnel idle timeout. Shutdown stops admission, closes active sockets, and waits for handlers. An upstream connection or tunnel failure increments that proxy's health failure count. An upstream HTTP error status or client disconnect does not mark that proxy unhealthy. After `HEALTH_FAILURE_THRESHOLD` consecutive upstream failures, the store marks the proxy unhealthy for `HEALTH_COOLDOWN_SECONDS`. A successful tunnel resets consecutive health failures.

## State

Redis uses these keys:

- `proxies:<provider>` is a Redis hash. It maps proxy IDs to JSON records. A record includes upstream credentials.
- `health:<proxy_id>` is a string with a TTL. It marks the proxy unhealthy until `HEALTH_COOLDOWN_SECONDS` expires.
- `affinity:<key>` is a string with a TTL. It binds a key to a proxy ID until `PROXY_AFFINITY_TTL` expires.
- `pool:rr_index:<provider-set>` is an integer. It stores the shared round-robin index.
- `stats:<proxy_id>` is a hash. It stores success, failure, byte, and consecutive-failure counters.
- `provider_stats:<provider>` is a hash. It stores durable provider success, failure, and byte counters.
- `latency:<proxy_id>` is a list. It holds up to 1000 recent CONNECT latency samples in microseconds.

The refresher replaces each provider hash atomically. It removes health, stats, and latency keys for proxies that leave the list. It keeps affinity keys, provider counters, and unrelated providers. Stats and latency expire 30 days after the last write. Provider counters have no TTL. SQLite stores equivalent state in one local database. SQLite keeps provider counters in `provider_stats`. It removes inactive proxy statistics after 30 days during refresh.

SQLite stores upstream credentials in its database. It restricts the database and WAL and SHM files to owner access when it opens them. Keep the SQLite directory writable only by the service account.

A `redis://` connection does not encrypt Redis traffic. Use `rediss://` or an encrypted link when Redis traffic crosses an untrusted network. Redis traffic includes stored upstream passwords.

The Python server stores `latency:<proxy_id>` as a sorted set with milliseconds as scores. The Go service reads both formats. It changes a legacy sorted set to a list when it records the next request for that proxy. The conversion keeps old samples and counters. Stop Python proxy writers before you use Go proxy writers with the same Redis instance.

## HTTP API

The API listens on `API_HOST:API_PORT`. It binds to `127.0.0.1` by default. It has no authentication. Use an external bind only behind trusted-client network controls. All routes are read-only. Successful GET routes except `/metrics` return JSON. An unknown route returns `404` with `{"error":"not found"}`. A store error returns `503` with `{"error":"store unavailable"}`. `GET /health` checks Redis or SQLite connectivity. A healthy connection does not prove that every stored record has a valid format.

- `GET /` returns the service name and an `endpoints` map of the other six paths.
- `GET /health` returns `{"status":"ok"}` when the store responds.
- `GET /stats` returns an array of current proxy statistics. It returns an empty array when no proxies exist.
- `GET /stats/{proxy_id}` returns one statistics object. It returns zero counts for an unknown ID.
- `GET /providers` returns total, healthy, and unhealthy counts by provider.
- `GET /routes` returns each route's `pattern`, `providers`, and `strategy`.
- `GET /metrics` returns Prometheus text with `Content-Type: text/plain; version=0.0.4; charset=utf-8`.

Each `/stats` array item contains `proxy_id`, `provider`, `success_count`, `failure_count`, `bytes_transferred`, `latency_count`, `latency_avg_ms`, `latency_p50_ms`, and `latency_p95_ms`. The detail route contains the same counts and latency fields without `proxy_id` or `provider`. The service records bytes copied in both tunnel directions. It records the upstream CONNECT handshake duration in milliseconds. The latency fields use at most 1000 recent samples.

The root index maps `health` to `/health`, `stats` to `/stats`, `stats_detail` to `/stats/{proxy_id}`, `providers` to `/providers`, `routes` to `/routes`, and `metrics` to `/metrics`. The API and CLI omit upstream user names and passwords. Proxy IDs and hosts remain visible.

## Prometheus metrics

Prometheus can scrape `GET /metrics` on the API listener. The response groups each metric with `# HELP` and `# TYPE` lines. The response ends with a line feed. The endpoint returns `503` without metric samples if the store read fails.

- `proxy_manager_proxies` is a gauge. It counts current proxies with `provider` and `health` labels.
- `proxy_manager_requests_total` is a counter. It counts recorded CONNECT requests with `provider` and `result` labels. The result is `success` or `failure`.
- `proxy_manager_bytes_transferred_total` is a counter. It counts bytes copied in both directions with a `provider` label.
- `proxy_manager_connect_duration_seconds` is a summary. Its `_sum` and `_count` samples record upstream CONNECT handshake durations with a `provider` label. The sum uses seconds. It has no quantile samples.

The service reads current store state for every scrape. It does not publish credentials or proxy IDs as labels. Provider counters and durations keep their values when a refresh removes proxies. A new provider starts with zero values. The service does not add old per-proxy statistics to new provider counters. Existing provider request counts do not enter the duration count.

Use these PromQL queries to see Rayobyte successes and failures per second:

```promql
rate(proxy_manager_requests_total{provider="rayobyte",result="success"}[5m])
rate(proxy_manager_requests_total{provider="rayobyte",result="failure"}[5m])
```

Use `increase(proxy_manager_requests_total{provider="rayobyte",result="failure"}[1h])` to count failures over one hour.

Use the sum and count rates to find the mean Rayobyte CONNECT handshake time in seconds over five minutes:

```promql
rate(proxy_manager_connect_duration_seconds_sum{provider="rayobyte"}[5m])
/
rate(proxy_manager_connect_duration_seconds_count{provider="rayobyte"}[5m])
```

The query needs a recorded handshake in the selected period. The duration counters start at zero when the service adds this metric to an existing store.
