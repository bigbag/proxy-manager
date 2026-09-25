# Deployment

This guide covers production topologies, configuration files, permissions, and shutdown behavior. Read `README.md` for the full environment variable list. Read `docs/architecture.md` for state layouts and API contracts.

## Topologies

### Single node with SQLite

Run one proxy process, one API process, and one refresher against one local SQLite file. The proxy and API processes may open the same file. Do not share the file between nodes. WAL mode needs a local filesystem, and the service is designed for one node per file.

```sh
STORE=sqlite SQLITE_PATH=/var/lib/proxy-manager/proxy.db ./bin/proxy_manager run proxy
STORE=sqlite SQLITE_PATH=/var/lib/proxy-manager/proxy.db ./bin/proxy_manager run api
STORE=sqlite SQLITE_PATH=/var/lib/proxy-manager/proxy.db ./bin/proxy_manager refresh
```

### Multiple nodes with Redis

Run one refresher for each shared Redis instance. Run one or more proxy nodes against that instance. Run one read-only API. The nodes share proxy lists, health, affinity, and statistics through Redis.

```sh
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 ./bin/proxy_manager refresh
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 ./bin/proxy_manager run proxy
STORE=redis REDIS_URL=redis://127.0.0.1:6379/0 ./bin/proxy_manager run api
```

Use `rediss://` when Redis traffic crosses a trust boundary. Stop any legacy Python writers before you share an existing instance. The Go client migrates old latency data on first read.

## Configuration files

| File | Purpose | Required |
|------|---------|----------|
| `providers.json` | Provider credentials and per-provider settings | No |
| `routes.json` | Host patterns to provider lists and strategies | No |
| `.local_env` | Environment variables for `make run/*` targets | No |

The service starts without these files. The default route sends all hosts to `webshare`, `proxyrack`, then `rayobyte` with `priority_fallback`. A `PROVIDERS_CONFIG_PATH` or `ROUTES_CONFIG_PATH` that you set explicitly must exist. A missing explicit file stops startup.

Git ignores all three files. They contain credentials. Do not commit them. Restrict permissions:

```sh
chmod 0600 providers.json .local_env
```

The SQLite store sets its database, WAL, and shared-memory files to `0600` on open. It refuses symlinked sidecar files. Keep `SQLITE_PATH` on a local filesystem.

## Shutdown

Send `SIGINT` or `SIGTERM`. The API drains with a five-second timeout. The proxy closes its listener, closes active tunnels, and waits for workers to finish. The refresher stops at the next tick. The service checks the store connection before it starts a listener, so a bad `REDIS_URL` fails fast at startup.

## Monitoring

The API serves Prometheus metrics at `/metrics` and a store health check at `/health`. Alert on `proxy_manager_requests_total{result="failure"}` growth per provider. See the metrics section in `docs/architecture.md` for the exact names and example queries.

## Security checklist

- The API and CONNECT server have no client authentication. Keep both on `127.0.0.1` or behind your own authentication layer.
- The client leg is plain TCP. The CONNECT request and target host are visible on the wire.
- Provider credentials travel as base64 Basic auth on the provider leg. The service speaks only plain TCP to upstream proxies. Provider-side TLS support does not protect this link. Keep the provider leg on a trusted network or wrap it in an external TLS tunnel, such as stunnel or a VPN.
- Logs never contain passwords or full credential-bearing URLs. The API output omits credentials.
- Run `make security` before each release. CI runs the same scan.
