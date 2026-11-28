#!/usr/bin/env python3
"""Smoke test real remote native Claude Code against Mnevo with configured local or remote models."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

from claude_integration import admin, memory_results, reference

if len(sys.argv) != 2:
    raise SystemExit("usage: claude_remote.py /path/to/mnevo")
binary = Path(sys.argv[1]).resolve()
work = Path(tempfile.mkdtemp(prefix="mnevo-remote-"))
print("Remote Claude artifacts:", work, flush=True)
home = work / "home"
claude_dir = work / "claude-config"
claude_dir.mkdir(mode=0o700)
admin(binary, home, "setup", "-claude-dir", str(claude_dir))

env = {k: v for k, v in os.environ.items() if not k.startswith("ANTHROPIC_BASE_URL")}
env.update(
    CLAUDE_CONFIG_DIR=str(claude_dir),
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
    DISABLE_NON_ESSENTIAL_MODEL_CALLS="1",
)


def turn(name, prompt, resume=None):
    args = ["claude"]
    if resume:
        args += ["--resume", resume]
    args += ["-p", "--max-turns", "4", "--tools", "", "--permission-mode", "dontAsk",
             "--allowedTools", "mcp__mnevo__get_memory", "--output-format", "stream-json",
             "--verbose", "--include-hook-events", prompt]
    proc = subprocess.run(args, cwd=work, env=env, text=True, capture_output=True, timeout=75)
    (work / (name + ".jsonl")).write_text(proc.stdout)
    (work / (name + ".stderr")).write_text(proc.stderr)
    rows = [json.loads(line) for line in proc.stdout.splitlines()]
    final = next((row for row in rows if row.get("type") == "result"), {})
    if proc.returncode or not final or final.get("is_error"):
        raise SystemExit(1)
    return rows, final


try:
    model_view = admin(binary, home, "model", "show")["model_config"]
    if not model_view["recall_ready"]:
        print("SKIP: configure MNEVO_EMBED_MODEL and MNEVO_JEV_MODEL to run remote recall check")
        raise SystemExit(0)
    draft = {"type": "preference", "summary": "周记优先写一句结论", "tags": ["周记"], "conditions": "撰写周记"}
    learned, _ = turn("learn", "记住：" + json.dumps(draft, ensure_ascii=False))
    assert any("Mnevo 已收到" in row.get("stdout", "") for row in learned)
    for _ in range(50):
        mems = admin(binary, home, "memory", "list", "周记").get("memories", [])
        if mems and mems[0]["embedded"]:
            break
        time.sleep(0.1)
    recalled, final = turn("read", "请写周记。先用 mnevo 的 get_memory 读取本次 Hook 引用，再复述记忆摘要。")
    assert any(item.get("status") == "ok" for item in memory_results(recalled))
    prev = reference(recalled)
    resumed, _ = turn("resume", "继续写周记并调用 get_memory。", final["session_id"])
    assert reference(resumed) != prev
finally:
    admin(binary, home, "stop")
print("PASS: remote native Claude Code formation, vector+Jev recall, and resumption")
