#!/usr/bin/env python3
"""Verify interactive TTY Stop lifecycle and SQLite old-reference revocation in native Claude Code."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time

from claude_integration import Script, admin, clean_env, configure_models, mock_server, service


def run(binary, work, home, api_url):
    config = work / "config"
    config.mkdir()
    (config / ".claude.json").write_text(json.dumps({
        "hasCompletedOnboarding": True, "theme": "dark",
        "projects": {str(work): {"hasTrustDialogAccepted": True}},
    }))
    assert admin(binary, home, "setup", "-claude-dir", str(config))["status"] == "ok"

    env = clean_env({
        "TERM": "xterm-256color",
        "ANTHROPIC_BASE_URL": api_url,
        "ANTHROPIC_API_KEY": "local-scripted-test",
        "CLAUDE_CONFIG_DIR": str(config),
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1",
    })
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 36, 120, 0, 0))
    options = [
        "claude",
        "--permission-mode", "dontAsk",
        "--allowedTools", "mcp__mnevo__get_memory,mcp__mnevo__submit_memory_feedback",
    ]
    proc = subprocess.Popen(options, cwd=work, env=env, stdin=slave, stdout=slave,
                            stderr=slave, start_new_session=True)
    os.close(slave)
    try:
        with (work / "terminal.log").open("wb") as log:
            def drain(seconds):
                deadline = time.monotonic() + seconds
                tail = b""
                while time.monotonic() < deadline:
                    if not select.select([master], [], [], 0.1)[0]:
                        continue
                    try:
                        data = os.read(master, 65536)
                    except OSError:
                        break
                    if not data:
                        break
                    tail = (tail + data)[-65536:]
                    if log.tell() < 256 << 10:
                        log.write(data)
                return tail

            def enter(text):
                os.write(master, text.encode())
                drain(0.45)
                os.write(master, b"\r")

            def until(predicate):
                for _ in range(60):
                    drain(0.25)
                    time.sleep(0.1)
                    rows = admin(binary, home, "audit").get("audit", [])
                    if predicate(rows):
                        return rows
                    if proc.poll() is not None:
                        raise RuntimeError("Claude Code exited; see terminal.log")
                raise TimeoutError("expected terminal decision not recorded; see terminal.log")

            startup = b""
            for _ in range(40):
                startup = (startup + drain(0.25))[-65536:]
                if b"custom" in startup and b"API" in startup:
                    os.write(master, b"\x1b[A\r")
                    break
                time.sleep(0.1)
            for _ in range(8):
                drain(0.25)
                time.sleep(0.1)

            enter("CASE_FORMED 写本周周报结论")
            rows = until(lambda entries:
                         any(row["operation"] == "Stop" for row in entries) and
                         any(row["operation"] == "get_memory" and row["result"] == "ok" for row in entries))
            delivery = next(row for row in rows if row["operation"] == "recall" and row["result"] == "delivered")
            assert any(row["operation"] == "Stop" and row["task_id"] == delivery["task_id"] for row in rows)
            drain(1)
            enter(f"CASE_LATE {delivery['reference']} 请查证上一轮引用")
            rows = until(lambda entries:
                         any(row["operation"] == "get_memory" and row["result"] == "unavailable" for row in entries))
            assert proc.poll() is None
            (work / "audit.json").write_text(json.dumps(rows, ensure_ascii=False, indent=2) + "\n")
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)
        os.close(master)


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: claude_tty.py /path/to/mnevo")
    binary = Path(sys.argv[1]).resolve()
    work = Path(tempfile.mkdtemp(prefix="mnevo-tty-"))
    print("Interactive Claude artifacts:", work, flush=True)
    home = work / "home"
    with mock_server(Script(work / "unused")) as api, service(binary, home):
        thread = threading.Thread(target=api.serve_forever, daemon=True)
        thread.start()
        try:
            url = f"http://127.0.0.1:{api.server_port}"
            configure_models(binary, home, url)
            draft = {"type": "preference", "summary": "周报开头先给三条核心结论",
                     "tags": ["周报", "结论"], "conditions": "撰写周报时"}
            admin(binary, home, "memory", "add", json.dumps(draft, ensure_ascii=False))
            run(binary, work, home, url)
        finally:
            api.shutdown()
            thread.join()
    print("PASS: native interactive Stop closed the task, and the old reference was revoked")


if __name__ == "__main__":
    main()
