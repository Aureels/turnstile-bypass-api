# Turnstile Bypass API

Headful Chrome renderer that clears Cloudflare's managed challenge and the
Turnstile widget, then returns the real page behind them.

Python 3.10+, `undetected-chromedriver`, and Xvfb on headless servers.

## Approach

Cloudflare rejects plain headless Chrome at two stages: the managed challenge
and the Turnstile checkbox, which lives in a cross-origin iframe and needs a
real input event. A patched Chrome (`undetected-chromedriver`) running headful
on a virtual X display passes both. The resulting `cf_clearance` is cached in a
persistent profile, so only the first navigation pays the full challenge cost.

## Requirements

- Python 3.10+
- Chromium or Google Chrome
- Xvfb for headful on a server

```bash
apt-get install -y chromium xvfb
export CHROME_PATH=/usr/bin/google-chrome
```

Chrome is resolved from `CHROME_PATH`, `/usr/local/bin/chromium`,
`/usr/bin/google-chrome*`, `/usr/bin/chromium*`, `/snap/bin/chromium`, or the
Playwright cache. The Chrome major version is read from the binary so the
matching driver is fetched automatically.

## Install

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
```

## Run

```bash
python3 run.py
```

`run.py` wraps the server in `xvfb-run` when `DISPLAY` is empty. Equivalent
manual start:

```bash
xvfb-run -a -s "-screen 0 1366x768x24" \
  python3 -m uvicorn app:app --host 0.0.0.0 --port 8080
```

Keep `HEADLESS=false`. Headless mode passes the managed challenge but not the
Turnstile widget.

## API

### POST /render

`POST /solve` is an alias.

```bash
curl -s -X POST http://localhost:8080/render \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com/","include_html":true,"include_text":true}'
```

| Field | Default | Description |
|---|---|---|
| `url` | required | http/https target |
| `wait_selector` | none | CSS selector to wait for |
| `wait_ms` | 0 | Extra settle time, max 15000 |
| `timeout_ms` | `NAV_TIMEOUT_SECONDS` | Per-request timeout |
| `include_html` | true | Browser DOM |
| `include_text` | false | Visible text |
| `include_cookies` | false | Cookies including `cf_clearance` |
| `include_screenshot` | false | PNG as base64 |
| `cookie` | empty | Raw `Cookie` header to preload |

Response:

```json
{
  "success": true,
  "data": {
    "url": "https://example.com/",
    "final_url": "https://example.com/",
    "status": 200,
    "title": "Example Domain",
    "challenge": "bypassed",
    "turnstile_token": "",
    "cf_clearance": "...",
    "html": "<!doctype html>...",
    "text": "Example Domain",
    "cookies": [],
    "screenshot_base64": "",
    "duration_ms": 12000,
    "truncated": false,
    "backend": "undetected-chromedriver",
    "error": null
  }
}
```

`challenge` is one of `none`, `bypassed`, `cloudflare-managed-challenge`,
`turnstile-widget`, `http-403`, `http-429`, `http-5xx`.

`success` is true for 2xx and 3xx responses, or whenever `cf_clearance` was
obtained.

### POST /batch

Array of render requests, maximum 10, executed sequentially on the shared
browser. Per-item failures appear in `meta.errors`.

### GET /health

Chrome path, display, headless flag, backend, uptime.

### GET /metrics

Counters, success rate, average latency, active renders, uptime.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PORT` | 8080 | Listen port |
| `NAV_TIMEOUT_SECONDS` | 180 | Navigation and challenge timeout |
| `HEADLESS` | false | Keep false for Turnstile |
| `PROXY` | none | `http://user:pass@host:port` |
| `USER_DATA_DIR` | `~/.cache/turnstile-bypass-api/chrome-profile` | Persistent profile |
| `CHROME_PATH` | auto | Chrome or Chromium binary |
| `MAX_HTML_BYTES` | 4194304 | HTML and text cap |
| `API_KEY` | none | Require `X-Api-Key` |
| `ALLOW_PRIVATE` | false | Allow private and loopback targets |

## CLI

```bash
xvfb-run -a python3 bypass.py --url https://example.com/ --output result.json
```

## Operations

One browser instance serializes requests through a lock. For concurrency, run
several processes with distinct `USER_DATA_DIR` and `PORT` values.

`cf_clearance` is bound to IP and user agent. Changing `PROXY` requires a fresh
profile, otherwise the first request re-challenges.

Set `API_KEY` and place the service behind a reverse proxy before exposing it.
Render only sites you own or are permitted to access.
