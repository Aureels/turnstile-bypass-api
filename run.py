import os
import shutil
import sys

port = os.environ.get("PORT", "8080")
command = [
    sys.executable,
    "-m",
    "uvicorn",
    "app:app",
    "--host",
    "0.0.0.0",
    "--port",
    port,
    "--workers",
    os.environ.get("WORKERS", "1"),
]
if not os.environ.get("DISPLAY") and shutil.which("xvfb-run"):
    command = ["xvfb-run", "-a", "-s", "-screen 0 1366x768x24"] + command
os.execvp(command[0], command)
