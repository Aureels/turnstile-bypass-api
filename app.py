import ipaddress
import os
import socket
import threading
import time
from contextlib import asynccontextmanager
from typing import Any, Dict, List, Optional
from urllib.parse import urlparse

from fastapi import FastAPI, Header, HTTPException
from fastapi.responses import JSONResponse
from pydantic import BaseModel

from bypass import CloudflareBypass, find_chrome

VERSION = "3.1.0"

PORT = int(os.getenv("PORT", "8080"))
API_KEY = os.getenv("API_KEY") or None
NAV_TIMEOUT = int(os.getenv("NAV_TIMEOUT_SECONDS", "180"))
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
ALLOW_PRIVATE = os.getenv("ALLOW_PRIVATE", "false").lower() in ("1", "true", "yes")

engine = CloudflareBypass(
    proxy=os.getenv("PROXY") or None,
    user_data_dir=os.getenv("USER_DATA_DIR") or None,
    timeout=NAV_TIMEOUT,
    headless=HEADLESS,
    max_html_bytes=int(os.getenv("MAX_HTML_BYTES", str(4 << 20))),
)

metrics_lock = threading.Lock()
metrics: Dict[str, Any] = {
    "started": time.time(),
    "requests": 0,
    "ok": 0,
    "failed": 0,
    "active": 0,
    "total_ms": 0,
}


def bump(key: str, delta: int = 1) -> None:
    with metrics_lock:
        metrics[key] = metrics.get(key, 0) + delta


class RenderRequest(BaseModel):
    url: str
    wait_selector: Optional[str] = None
    wait_ms: int = 0
    timeout_ms: Optional[int] = None
    include_html: bool = True
    include_text: bool = False
    include_cookies: bool = False
    include_screenshot: bool = False
    cookie: str = ""


def is_private(url: str) -> bool:
    if ALLOW_PRIVATE:
        return False
    host = (urlparse(url).hostname or "").strip()
    if not host:
        return True
    try:
        ipaddress.ip_address(host)
        addresses = {host}
    except ValueError:
        try:
            addresses = {info[4][0] for info in socket.getaddrinfo(host, None)}
        except OSError:
            return False
    for address in addresses:
        try:
            ip = ipaddress.ip_address(address)
        except ValueError:
            continue
        if ip.is_private or ip.is_loopback or ip.is_link_local or ip.is_multicast or ip.is_unspecified:
            return True
    return False


def authorize(key: Optional[str]) -> None:
    if API_KEY and key != API_KEY:
        raise HTTPException(status_code=401, detail="invalid api key")


def run(req: RenderRequest) -> Dict[str, Any]:
    if is_private(req.url):
        raise HTTPException(status_code=400, detail="target url is not allowed")
    bump("requests")
    bump("active")
    try:
        result = engine.fetch(
            req.url,
            wait_selector=req.wait_selector,
            wait_ms=req.wait_ms,
            include_html=req.include_html,
            include_text=req.include_text,
            include_cookies=req.include_cookies,
            include_screenshot=req.include_screenshot,
            cookie_header=req.cookie,
            timeout=(req.timeout_ms // 1000) if req.timeout_ms else None,
        )
    finally:
        bump("active", -1)
    if result.get("error"):
        bump("failed")
    elif 200 <= result.get("status", 0) < 400 or result.get("cf_clearance"):
        bump("ok")
        bump("total_ms", result.get("duration_ms", 0))
    else:
        bump("failed")
    return result


@asynccontextmanager
async def lifespan(_: FastAPI):
    yield
    engine.close()


app = FastAPI(title="Turnstile Bypass API", version=VERSION, lifespan=lifespan)


@app.get("/health")
def health() -> Dict[str, Any]:
    with metrics_lock:
        uptime = int(time.time() - metrics["started"])
    return {
        "success": True,
        "data": {
            "status": "healthy",
            "version": VERSION,
            "uptime_seconds": uptime,
            "chrome_path": find_chrome(),
            "display": os.getenv("DISPLAY"),
            "headless": engine.headless,
            "backend": "undetected-chromedriver",
        },
    }


@app.get("/metrics")
def stats() -> Dict[str, Any]:
    with metrics_lock:
        requests = metrics["requests"]
        ok = metrics["ok"]
        data = {
            "total_requests": requests,
            "successful_renders": ok,
            "failed_renders": metrics["failed"],
            "success_rate": (ok / requests * 100.0) if requests else 0.0,
            "active_renders": metrics["active"],
            "avg_latency_ms": (metrics["total_ms"] // ok) if ok else 0,
            "uptime_seconds": int(time.time() - metrics["started"]),
        }
    return {"success": True, "data": data}


@app.post("/render")
@app.post("/solve")
def render(req: RenderRequest, x_api_key: Optional[str] = Header(default=None)) -> JSONResponse:
    authorize(x_api_key)
    try:
        result = run(req)
    except HTTPException as exc:
        return JSONResponse(status_code=exc.status_code, content={"success": False, "error": exc.detail})
    except Exception as exc:
        return JSONResponse(status_code=502, content={"success": False, "error": f"{type(exc).__name__}: {exc}"})
    good = 200 <= result.get("status", 0) < 400 or bool(result.get("cf_clearance"))
    return JSONResponse(status_code=200, content={"success": good, "data": result})


@app.post("/batch")
def batch(requests: List[RenderRequest], x_api_key: Optional[str] = Header(default=None)) -> JSONResponse:
    authorize(x_api_key)
    if not requests:
        return JSONResponse(status_code=400, content={"success": False, "error": "empty batch"})
    if len(requests) > 10:
        return JSONResponse(status_code=400, content={"success": False, "error": "batch size max 10"})
    data: List[Optional[Dict[str, Any]]] = []
    errors: List[str] = []
    for req in requests:
        try:
            data.append(run(req))
            errors.append("")
        except HTTPException as exc:
            data.append(None)
            errors.append(str(exc.detail))
        except Exception as exc:
            data.append(None)
            errors.append(f"{type(exc).__name__}: {exc}")
    return JSONResponse(status_code=200, content={"success": True, "data": data, "meta": {"errors": errors}})
