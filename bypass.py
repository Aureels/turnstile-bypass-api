import atexit, glob, os, re, shutil, subprocess, sys, threading, time
from pathlib import Path
from typing import Any, Dict, List, Optional

import undetected_chromedriver as uc
from selenium.common.exceptions import NoSuchElementException, StaleElementReferenceException, TimeoutException
from selenium.webdriver.common.action_chains import ActionChains
from selenium.webdriver.common.by import By
from selenium.webdriver.support import expected_conditions as EC
from selenium.webdriver.support.ui import WebDriverWait

CHROME_CANDIDATES = [
    "/usr/local/bin/chromium", "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium", "/usr/bin/chromium-browser", "/snap/bin/chromium",
    str(Path.home() / ".cache/ms-playwright/chromium-*/chrome-linux*/chrome"),
    str(Path.home() / ".cache/ms-playwright/chromium-*/chrome-linux64/chrome"),
]
CHECKBOX_SELECTORS = [
    'input[type="checkbox"]', "input#checkbox", "#checkbox", ".cf-turnstile-checkbox",
    '[role="checkbox"]', "div#cf-stage .cb-lb input", "label input",
]
FRAME_SELECTORS = [
    'iframe[src*="challenges.cloudflare.com"]', 'iframe[src*="turnstile"]',
    'iframe[title*="Cloudflare"]', 'iframe[id^="cf-chl-widget"]',
]
DEFAULT_PROFILE = Path.home() / ".cache" / "turnstile-bypass-api" / "chrome-profile"
FLAGS = (
    "--no-sandbox", "--disable-dev-shm-usage", "--disable-gpu", "--window-size=1366,768",
    "--lang=id-ID", "--disable-blink-features=AutomationControlled",
    "--disable-features=IsolateOrigins,site-per-process,TranslateUI",
    "--profile-directory=Default", "--no-first-run", "--no-default-browser-check",
    "--hide-scrollbars", "--mute-audio", "--disable-background-timer-throttling",
    "--disable-renderer-backgrounding", "--disable-ipc-flooding-protection",
)


def log(message: str) -> None:
    print(f"[bypass] {message}", file=sys.stderr, flush=True)


def find_chrome() -> Optional[str]:
    env = os.getenv("CHROME_PATH")
    if env and Path(env).is_file():
        return env
    for pattern in CHROME_CANDIDATES:
        for path in sorted(glob.glob(pattern), reverse=True):
            if Path(path).is_file() and os.access(path, os.X_OK):
                return path
    for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
        if found := shutil.which(name):
            return found
    return None


def chrome_major(binary: str) -> Optional[int]:
    try:
        out = subprocess.run([binary, "--version"], capture_output=True, text=True, timeout=15).stdout
        match = re.search(r"(\d+)\.\d+\.\d+\.\d+", out) or re.search(r"(\d+)\.", out)
        return int(match.group(1)) if match else None
    except Exception:
        return None


class Display:
    def __init__(self) -> None:
        self.process: Optional[subprocess.Popen] = None

    def ensure(self) -> Optional[str]:
        if os.getenv("DISPLAY"):
            return os.environ["DISPLAY"]
        if not shutil.which("Xvfb"):
            return None
        for num in range(99, 120):
            display = f":{num}"
            if Path(f"/tmp/.X11-unix/X{num}").exists():
                continue
            process = subprocess.Popen(
                ["Xvfb", display, "-screen", "0", "1366x768x24", "-ac"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            time.sleep(0.6)
            if process.poll() is None:
                self.process = process
                os.environ["DISPLAY"] = display
                atexit.register(self.stop)
                return display
        return None

    def stop(self) -> None:
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except Exception:
                self.process.kill()
        self.process = None


def extract_text(html: str) -> str:
    text = re.sub(r"<script[^>]*>.*?</script>", " ", html, flags=re.S | re.I)
    text = re.sub(r"<style[^>]*>.*?</style>", " ", text, flags=re.S | re.I)
    text = re.sub(r"<[^>]+>", " ", text)
    text = text.replace("&nbsp;", " ").replace("&lt;", "<").replace("&gt;", ">").replace("&amp;", "&")
    return re.sub(r"\s+", " ", text).strip()


def detect_challenge(html: str, status: int) -> str:
    low = html.lower()
    interstitial = "just a moment" in low or "cf_chl_opt" in low
    if "cf-chl-widget" in low or "cf-turnstile-response" in low:
        return "cloudflare-managed-challenge" if interstitial else "turnstile-widget"
    if interstitial:
        return "cloudflare-managed-challenge"
    if "cf-turnstile" in low or "challenges.cloudflare.com/turnstile" in low:
        return "turnstile-widget"
    if status == 403:
        return "http-403"
    if status == 429:
        return "http-429"
    if status >= 500:
        return f"http-{status}"
    return "none"


class CloudflareBypass:
    def __init__(self, chrome_path=None, proxy=None, user_data_dir=None, timeout=180, headless=False, max_html_bytes=4 << 20):
        self.chrome_path = chrome_path or find_chrome()
        self.proxy = proxy or None
        self.profile = str(user_data_dir or DEFAULT_PROFILE)
        self.timeout, self.headless, self.max_html_bytes = timeout, headless, max_html_bytes
        self.driver: Optional[uc.Chrome] = None
        self.lock = threading.Lock()
        self.display = Display()
        self.major = chrome_major(self.chrome_path) if self.chrome_path else None

    def options(self) -> uc.ChromeOptions:
        opts = uc.ChromeOptions()
        if self.headless:
            opts.add_argument("--headless=new")
        for flag in FLAGS + (f"--user-data-dir={self.profile}",):
            opts.add_argument(flag)
        if self.chrome_path:
            opts.binary_location = self.chrome_path
        if self.proxy:
            opts.add_argument(f"--proxy-server={self.proxy}")
        return opts

    def ensure_driver(self) -> uc.Chrome:
        if self.driver is not None:
            try:
                self.driver.current_url
                return self.driver
            except Exception:
                self.close()
        Path(self.profile).mkdir(parents=True, exist_ok=True)
        if not self.headless and self.display.ensure() is None:
            log("no DISPLAY; falling back to headless")
            self.headless = True
        kwargs: Dict[str, Any] = {"options": self.options(), "use_subprocess": True}
        if self.major:
            kwargs["version_main"] = self.major
        log(f"chrome={self.chrome_path} headless={self.headless} display={os.getenv('DISPLAY')} proxy={self.proxy or '-'}")
        driver = uc.Chrome(**kwargs)
        driver.set_page_load_timeout(self.timeout)
        driver.implicitly_wait(0)
        try:
            driver.execute_cdp_cmd(
                "Page.addScriptToEvaluateOnNewDocument",
                {"source": "Object.defineProperty(navigator,'webdriver',{get:()=>undefined});"},
            )
        except Exception:
            pass
        self.driver = driver
        return driver

    def close(self) -> None:
        if self.driver:
            try:
                self.driver.quit()
            except Exception:
                pass
            self.driver = None
        self.display.stop()

    def clearance(self, driver: uc.Chrome) -> Optional[str]:
        return next(
            (c.get("value", "") for c in driver.get_cookies() if c.get("name", "").startswith("cf_clearance")),
            None,
        )

    def wait_clearance(self, driver: uc.Chrome, max_wait: int) -> Optional[str]:
        deadline = time.time() + max_wait
        while time.time() < deadline:
            if value := self.clearance(driver):
                log("cf_clearance acquired")
                return value
            try:
                page = driver.page_source or ""
                if "cf_chl_opt" not in page and "challenge-platform" not in page:
                    return None
            except Exception:
                pass
            time.sleep(1.5)
        log("cf_clearance timeout")
        return None

    def token(self, driver: uc.Chrome) -> str:
        try:
            return driver.execute_script(
                "const e=document.querySelector('input[name=\"cf-turnstile-response\"],"
                "textarea[name=\"cf-turnstile-response\"]');return e?e.value:'';"
            ) or ""
        except Exception:
            return ""

    def frames(self, driver: uc.Chrome) -> List[Any]:
        found, seen = [], set()
        for selector in FRAME_SELECTORS:
            try:
                frames = driver.find_elements(By.CSS_SELECTOR, selector)
            except Exception:
                continue
            for frame in frames:
                key = getattr(frame, "id", None)
                if key and key not in seen:
                    seen.add(key)
                    found.append(frame)
        return found

    def click_checkbox(self, driver: uc.Chrome) -> bool:
        frames = self.frames(driver)
        for index, frame in enumerate(frames):
            try:
                driver.switch_to.frame(frame)
            except Exception:
                driver.switch_to.default_content()
                continue
            clicked = False
            for selector in CHECKBOX_SELECTORS:
                try:
                    element = WebDriverWait(driver, 2).until(EC.element_to_be_clickable((By.CSS_SELECTOR, selector)))
                    driver.execute_script("arguments[0].scrollIntoView({block:'center'})", element)
                    time.sleep(0.3)
                    try:
                        element.click()
                    except Exception:
                        ActionChains(driver).move_to_element(element).click().perform()
                    clicked = True
                    log(f"checkbox clicked in frame {index} via {selector}")
                    break
                except (TimeoutException, NoSuchElementException, StaleElementReferenceException):
                    continue
                except Exception:
                    continue
            if not clicked:
                try:
                    ActionChains(driver).move_to_element(driver.find_element(By.TAG_NAME, "body")).click().perform()
                    clicked = True
                    log(f"frame {index} body clicked")
                except Exception:
                    pass
            driver.switch_to.default_content()
            if clicked:
                time.sleep(2.5)
                if self.token(driver):
                    return True
        return False

    def solve_turnstile(self, driver: uc.Chrome, max_wait: int) -> str:
        deadline = time.time() + max_wait
        while time.time() < deadline:
            if value := self.token(driver):
                log("turnstile token acquired")
                return value
            try:
                if "Just a moment" not in (driver.title or "") and "challenge-platform" not in (driver.page_source or ""):
                    return ""
                self.click_checkbox(driver)
            except Exception as exc:
                log(f"turnstile error: {exc}")
            time.sleep(2)
        log("turnstile timeout")
        return ""

    def wait_ready(self, driver: uc.Chrome, max_wait: int) -> None:
        deadline = time.time() + max_wait
        while time.time() < deadline:
            try:
                if "Just a moment" not in (driver.title or "") and "__cf_chl" not in (driver.current_url or ""):
                    return
            except Exception:
                pass
            time.sleep(1)

    def settle(self, driver: uc.Chrome, url: str, timeout: int):
        clearance = self.wait_clearance(driver, timeout) or ""
        token = self.solve_turnstile(driver, max(30, timeout // 2))
        self.wait_ready(driver, min(45, timeout))
        if "Just a moment" in (driver.title or ""):
            driver.get(url)
            clearance = self.wait_clearance(driver, min(60, timeout)) or clearance
            token = self.solve_turnstile(driver, min(60, timeout // 2)) or token
            self.wait_ready(driver, min(45, timeout))
        return clearance, token

    def status(self, driver: uc.Chrome) -> int:
        try:
            return int(
                driver.execute_script(
                    "const n=performance.getEntriesByType('navigation')[0];"
                    "return n&&n.responseStatus?n.responseStatus:0;"
                ) or 0
            )
        except Exception:
            return 0

    def fetch(
        self, url: str, wait_selector: Optional[str] = None, wait_ms: int = 0,
        include_html: bool = True, include_text: bool = True, include_cookies: bool = False,
        include_screenshot: bool = False, cookie_header: str = "", timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        timeout = timeout or self.timeout
        started = time.time()
        result: Dict[str, Any] = {
            "url": url, "final_url": url, "status": 0, "title": "", "challenge": "none",
            "turnstile_token": "", "cf_clearance": "", "html": "", "text": "", "cookies": [],
            "screenshot_base64": "", "duration_ms": 0, "truncated": False,
            "backend": "undetected-chromedriver", "error": None,
        }
        with self.lock:
            try:
                driver = self.ensure_driver()
                if cookie_header:
                    driver.get(url)
                    for part in cookie_header.split(";"):
                        if "=" in part:
                            name, value = part.split("=", 1)
                            try:
                                driver.add_cookie({"name": name.strip(), "value": value.strip()})
                            except Exception:
                                pass
                driver.get(url)
                result["cf_clearance"], result["turnstile_token"] = self.settle(driver, url, timeout)
                if wait_selector:
                    try:
                        WebDriverWait(driver, 15).until(EC.presence_of_element_located((By.CSS_SELECTOR, wait_selector)))
                    except Exception:
                        pass
                time.sleep(min(wait_ms, 15000) / 1000.0 if wait_ms > 0 else 0.5)
                html = driver.page_source or ""
                result["final_url"] = driver.current_url
                result["title"] = driver.title or ""
                result["status"] = self.status(driver) or (403 if "Just a moment" in result["title"] else 200)
                result["challenge"] = detect_challenge(html, result["status"])
                result["cf_clearance"] = result["cf_clearance"] or (self.clearance(driver) or "")
                if result["cf_clearance"] and "Just a moment" not in result["title"]:
                    result["challenge"] = "bypassed"
                result["turnstile_token"] = result["turnstile_token"] or self.token(driver)
                if include_html:
                    if len(html) > self.max_html_bytes:
                        html, result["truncated"] = html[: self.max_html_bytes], True
                    result["html"] = html
                if include_text:
                    result["text"] = extract_text(html)[: self.max_html_bytes]
                if include_cookies:
                    result["cookies"] = driver.get_cookies()
                if include_screenshot:
                    try:
                        result["screenshot_base64"] = driver.get_screenshot_as_base64()
                    except Exception:
                        pass
            except Exception as exc:
                result["error"] = f"{type(exc).__name__}: {exc}"
                try:
                    if self.driver:
                        result["final_url"] = self.driver.current_url
                        result["title"] = self.driver.title or ""
                        result["html"] = self.driver.page_source or ""
                        result["text"] = extract_text(result["html"])
                        result["challenge"] = detect_challenge(result["html"], result["status"])
                except Exception:
                    pass
        result["duration_ms"] = int((time.time() - started) * 1000)
        return result


def main() -> None:
    import argparse, json

    parser = argparse.ArgumentParser(description="Cloudflare and Turnstile bypass")
    parser.add_argument("--url", default="https://example.com/")
    parser.add_argument("--headless", action="store_true")
    parser.add_argument("--proxy", default=os.getenv("PROXY") or None)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--user-data-dir", default=None)
    parser.add_argument("--output", default=None)
    args = parser.parse_args()
    engine = CloudflareBypass(
        proxy=args.proxy, timeout=args.timeout, headless=args.headless, user_data_dir=args.user_data_dir
    )
    try:
        result = engine.fetch(args.url, include_cookies=True)
    finally:
        engine.close()
    payload = json.dumps(result, indent=2, ensure_ascii=False)
    if args.output:
        Path(args.output).write_text(payload, encoding="utf-8")
        log(f"saved {args.output}")
    else:
        print(payload, flush=True)


if __name__ == "__main__":
    main()
