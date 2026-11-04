# Turnstile Bypass API

High-throughput Cloudflare Turnstile solver with proxy rotation, external solver integration, **automatic page fetch**, and horizontal scaling support. Built for production loads — thousands of requests/minute.

## Features

- **Multi-solver support**: 2Captcha, CapMonster, custom endpoint, direct verification
- **Proxy rotation**: HTTP/HTTPS/SOCKS5 with health checks, automatic failover
- **High concurrency**: Worker pool with configurable limits (default 1000 concurrent)
- **Rate limiting**: Token bucket per IP + global limits
- **Async + batch API**: Fire-and-forget or bulk solving
- **Auto-fetch**: Optional `fetch: true` returns full page content (HTML/JSON) in one call
- **Metrics**: Built-in Prometheus-compatible `/metrics` endpoint
- **Horizontal scaling**: Stateless, runs behind load balancer
- **Zero dependencies**: Single binary, no Docker required

## Quick Start

### 1. Configure

```bash
cp .env.example .env
# Edit .env with your keys and target
```

### 2. Add Proxies

```bash
# Edit proxies.txt with your proxy list
# Format: scheme://user:pass@host:port
vim proxies.txt
```

### 3. Run

```bash
# Direct (no Docker)
go run main.go

# Or build
go build -o turnstile-bypass && ./turnstile-bypass
```

### 4. Test

```bash
# Health check
curl http://localhost:8080/health

# Solve single Turnstile (returns token only)
curl -X POST http://localhost:8080/solve \
  -H "Content-Type: application/json" \
  -d '{
    "sitekey": "YOUR_SITE_KEY",
    "pageurl": "https://example.com/",
    "action": "login"
  }'

# Solve + Fetch (returns full page content in one call!)
curl -X POST http://localhost:8080/solve \
  -H "Content-Type: application/json" \
  -d '{
    "sitekey": "YOUR_SITE_KEY",
    "pageurl": "https://example.com/",
    "fetch": true
  }'

# Async solve (returns task_id immediately)
curl -X POST http://localhost:8080/solve \
  -H "Content-Type: application/json" \
  -d '{"sitekey": "...", "pageurl": "...", "async": true}'

# Check async result
curl http://localhost:8080/solve/<task_id>

# Batch solve (up to 100)
curl -X POST http://localhost:8080/batch \
  -H "Content-Type: application/json" \
  -d '[{"sitekey": "...", "pageurl": "..."}, {"sitekey": "...", "pageurl": "..."}]'

# Metrics
curl http://localhost:8080/metrics

# Proxy status
curl http://localhost:8080/proxies
```

## Auto-Fetch (Get Protected Page Content in One Call)

Add `"fetch": true` to your solve request — the API will solve Turnstile **then immediately fetch the protected page** using the token. Returns HTML/JSON directly.

**Request:**
```json
{
  "sitekey": "YOUR_SITE_KEY",
  "pageurl": "https://example.com/",
  "fetch": true,
  "fetch_method": "POST",
  "fetch_body": "",
  "fetch_headers": {}
}
```

**Response (fetch: true):**
```json
{
  "success": true,
  "data": {
    "task_id": "uuid",
    "token": "0.xxxxx...",
    "status": "completed",
    "solver_used": "2captcha",
    "latency_ms": 5200,
    "proxy_used": "http://user:pass@1.2.3.4:8080",
    "page_content": "<html>...protected page content...</html>",
    "content_type": "text/html; charset=utf-8",
    "status_code": 200,
    "headers": {
      "content-type": "text/html; charset=utf-8",
      "server": "cloudflare"
    }
  },
  "error": null
}
```

**Default fetch behavior:**
- Method: `POST` to the same `pageurl`
- Body: `cf-turnstile-response=<token>` (form-encoded)
- Headers: `User-Agent`, `Origin`, `Referer` set automatically

**Custom fetch (e.g., API endpoint):**
```json
{
  "sitekey": "YOUR_SITE_KEY",
  "pageurl": "https://api.example.com/protected",
  "fetch": true,
  "fetch_method": "GET",
  "fetch_headers": {
    "Authorization": "Bearer your-token",
    "Accept": "application/json"
  }
}
```

## API Reference

### POST /solve

Solve a Turnstile challenge. With `fetch: true`, also fetches the protected page content.

`sitekey` and `pageurl` are supplied per request. Setting `SITE_KEY` / `TARGET_URL` in the environment is optional and only acts as a fallback when a request omits them.

**Request:**
```json
{
  "sitekey": "YOUR_SITE_KEY",
  "pageurl": "https://example.com/",
  "action": "login",
  "cdata": "custom-data",
  "proxy": "http://user:pass@host:port",
  "async": false,
  "fetch": false,
  "fetch_method": "POST",
  "fetch_body": "",
  "fetch_headers": {}
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `sitekey` | yes* | Turnstile site key |
| `pageurl` | yes* | Absolute page URL carrying the widget |
| `action` | no | Turnstile action |
| `cdata` | no | Turnstile cData |
| `proxy` | no | Per-request proxy override |
| `priority` | no | Reserved, not implemented |
| `async` | no | Return `task_id` immediately |
| `callback_url` | no | Reserved, not implemented |
| `fetch` | no | Fetch the protected page after solving |
| `fetch_method` | no | `GET` or `POST` (default `POST`) |
| `fetch_body` | no | Custom request body |
| `fetch_headers` | no | Extra request headers |

\* or provided by the `SITE_KEY` / `TARGET_URL` environment defaults.

**Response (sync, fetch: false):**
```json
{
  "success": true,
  "data": {
    "task_id": "uuid",
    "token": "turnstile-token-here",
    "status": "completed",
    "solver_used": "2captcha",
    "latency_ms": 4500,
    "proxy_used": "http://user:pass@1.2.3.4:8080"
  },
  "error": null
}
```

**Response (sync, fetch: true):**
```json
{
  "success": true,
  "data": {
    "task_id": "uuid",
    "token": "0.xxxxx...",
    "status": "completed",
    "solver_used": "2captcha",
    "latency_ms": 5200,
    "proxy_used": "http://user:pass@1.2.3.4:8080",
    "page_content": "<html>...protected page content...</html>",
    "content_type": "text/html; charset=utf-8",
    "status_code": 200,
    "headers": {
      "content-type": "text/html; charset=utf-8",
      "server": "cloudflare"
    }
  },
  "error": null
}
```

**Response (async):**
```json
{
  "success": true,
  "data": {
    "task_id": "uuid",
    "status": "queued"
  }
}
```

### GET /solve/:task_id

Check async task status. Returns `page_content` if `fetch: true` was set.

### POST /batch

Solve up to 100 challenges in parallel. Each request supports `fetch` fields.

**Request:** Array of solve requests
**Response:** Array of solve responses (with `page_content` if `fetch: true`)

### GET /health

Health check endpoint.

### GET /metrics

Prometheus-style metrics:
- `total_requests`
- `successful_solves`
- `failed_solves`
- `success_rate`
- `active_workers`
- `proxies_total`
- `proxies_alive`
- `avg_latency_ms`
- `requests_per_second`

### GET /proxies

List proxies with status. Query params: `limit` (default 200), `alive=true|1` (alive only).

```bash
curl "http://localhost:8080/proxies?limit=50&alive=true"
```

### POST /proxies/reload

Reload proxies from file (hot reload, no restart).

## Solver Priority

1. **2Captcha** (if `TWOCAPTCHA_KEY` set) — most reliable
2. **CapMonster** (if `CAPMONSTER_KEY` set) — cheaper, good throughput
3. **Custom** (if `CUSTOM_SOLVER_URL` set) — your own solver
4. **Direct** — attempts direct verification (rarely works)

## Proxies

`proxies.txt` is preloaded with ~11k entries mirrored from
[proxifly/free-proxy-list](https://github.com/proxifly/free-proxy-list)
(`http`, `https`, `socks4`, `socks5` protocol feeds, deduplicated).

Free proxies churn fast. Refresh the list before a run:

```bash
./scripts/refresh-proxies.sh
curl -X POST http://localhost:8080/proxies/reload
```

Format (one per line):

```
# HTTP / HTTPS
http://user:pass@host:port
http://host:port
https://user:pass@host:port

# SOCKS5 (remote DNS — hostname resolved at the proxy)
socks5h://user:pass@host:port

# SOCKS5 (local DNS)
socks5://user:pass@host:port

# SOCKS4 / SOCKS4a
socks4://host:port
socks4a://host:port
```

Behavior:
- Startup runs an immediate health sweep, then every `HEALTH_CHECK_INTERVAL` seconds.
- Sweep hits `http://www.gstatic.com/generate_204` through each proxy with `HEALTH_CHECK_CONCURRENCY` workers.
- A proxy is dropped after 5 consecutive failures and re-added automatically when it recovers.
- Round-robin selection is O(1) over the alive set, safe for thousands of requests/second.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `:8080` | HTTP listen address |
| `PROXIES_FILE` | `./proxies.txt` | Proxy list path |
| `HEALTH_CHECK_INTERVAL` | `300` | Seconds between proxy sweeps (`0` = disabled) |
| `HEALTH_CHECK_CONCURRENCY` | `300` | Parallel checks per sweep |
| `SITE_KEY` | *(empty)* | Optional default site key; request value wins |
| `TARGET_URL` | *(empty)* | Optional default page URL; request value wins |
| `MAX_WORKERS` | `1000` | Max concurrent solves |
| `RATE_LIMIT` | `100` | Requests/second global limit |
| `BURST_LIMIT` | `200` | Burst allowance |
| `ENABLE_METRICS` | `true` | Enable metrics endpoint |
| `TWOCAPTCHA_KEY` | `` | 2Captcha API key |
| `CAPMONSTER_KEY` | `` | CapMonster API key |
| `CUSTOM_SOLVER_URL` | `` | Custom solver POST endpoint |
| `DEBUG` | `false` | Debug logging |

## Scaling

Run multiple instances behind any load balancer (nginx, traefik, HAProxy, cloud LB):

```bash
# Terminal 1
PORT=:8081 go run main.go

# Terminal 2
PORT=:8082 go run main.go

# Terminal 3
PORT=:8083 go run main.go

# nginx upstream example:
# upstream turnstile {
#   server 127.0.0.1:8081;
#   server 127.0.0.1:8082;
#   server 127.0.0.1:8083;
# }
```

Each instance is stateless — share the same `proxies.txt` and env vars.

## Performance Tuning

| Scenario | MAX_WORKERS | RATE_LIMIT | Proxies |
|----------|-------------|------------|---------|
| Light (100 req/min) | 50 | 10 | 10-20 |
| Medium (1k req/min) | 200 | 50 | 50-100 |
| Heavy (10k req/min) | 1000 | 200 | 200-500 |
| Extreme (100k req/min) | 5000 | 1000 | 1000+ |

**Notes:**
- Each worker holds one proxy connection
- 2Captcha/CapMonster have their own rate limits
- Monitor `avg_latency_ms` — if >10s, add more proxies or workers
- Use `socks5h` for DNS-through-proxy (avoids DNS leaks)

## Monitoring

### Prometheus Scrape Config

```yaml
scrape_configs:
- job_name: 'turnstile-bypass'
  static_configs:
  - targets: ['localhost:8080', 'localhost:8081', 'localhost:8082']
```

### Key Metrics to Alert On

- `turnstile_failed_solves_total` / `turnstile_total_requests_total` > 0.2 → high failure rate
- `turnstile_proxies_alive == 0` → no working proxies
- `turnstile_avg_latency_ms > 15000` → solver/proxy latency too high

## Troubleshooting

| Issue | Cause | Fix |
|-------|-------|-----|
| `no proxies available` | Proxies file empty or all dead | Check `proxies.txt`, run health check |
| `turnstile solving failed` | All solvers failed | Verify API keys, check solver balance |
| `rate limited` | Target returns 429 | Increase proxies, reduce RATE_LIMIT |
| High latency | Slow proxies or solver queue | Add faster proxies, use CapMonster |
| Memory growth | Task cache not cleaning | Reduce `CACHE_TTL`, check goroutine leaks |

## License

MIT — see [LICENSE](LICENSE).

## Disclaimer

For authorized testing only. Bypassing Turnstile on sites you don't own may violate Terms of Service. You are responsible for compliance with applicable laws and target site policies.