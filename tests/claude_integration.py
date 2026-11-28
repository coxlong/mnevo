#!/usr/bin/env python3
"""End-to-end integration test driving real native Claude Code processes against Mnevo's
unified HTTP-over-Unix-Socket daemon, `mnevo setup`, auto-start lifecycle, and Dual-System
pipeline (Embedding + Jev + LLM)."""
import argparse
import contextlib
import http.server
import json
import math
import os
from pathlib import Path
import queue
import re
import subprocess
import tempfile
import threading
import time
import urllib.request
import uuid

TIMEOUT = 60
REFERENCE = re.compile(r"记忆引用\s+([a-f0-9]{64})")
CASE = re.compile(r"CASE_([A-Z]+)(?:\s+([a-f0-9]{64}))?")


def tool(name, arguments):
    return "mcp__mnevo__" + name, arguments


def topic_vector(text):
    lower = text.lower()
    vec = [0.0, 0.0, 0.0, 0.0, 0.0]
    if any(k in lower for k in ("go", "context", "编程", "规范")):
        vec[0] += 1.0
    if any(k in lower for k in ("退款", "支付", "优惠券", "结算")):
        vec[1] += 1.0
    if any(k in lower for k in ("周报", "结论", "报告")):
        vec[2] += 1.0
    if any(k in lower for k in ("导出", "源表", "图标")):
        vec[3] += 1.0
    if sum(vec) == 0.0:
        vec[4] = 1.0
    norm = math.sqrt(sum(x * x for x in vec))
    return [x / norm for x in vec]


def clean_env(extra=None):
    env = {
        k: v for k, v in os.environ.items()
        if not k.startswith(("ANTHROPIC_", "MNEVO_EMBED_", "MNEVO_JEV_", "MNEVO_LLM_"))
    }
    if extra:
        env.update(extra)
    return env


class Script:
    def __init__(self, doc_file):
        self.doc_file = doc_file
        self.events = queue.Queue()
        self.gates = {"hold": threading.Event()}

    def wait(self, name):
        observed = self.events.get(timeout=TIMEOUT)
        assert observed == name, (name, observed)

    def pause(self, name):
        self.events.put(name)
        if not self.gates[name].wait(TIMEOUT):
            raise TimeoutError(name)

    def next_action(self, body):
        messages = body.get("messages", [])
        selected = None
        for index, message in enumerate(messages):
            if message["role"] != "user":
                continue
            content = message["content"]
            blocks = content if isinstance(content, list) else [{"type": "text", "text": content}]
            for block in blocks:
                if block.get("type") == "text" and (match := CASE.search(block.get("text", ""))):
                    selected = index, match
        if selected is None:
            return None
        index, match = selected
        name, forwarded = match.groups()
        turn = messages[index:]
        refs = REFERENCE.findall(json.dumps(turn, ensure_ascii=False))
        ref = refs[-1] if refs else None
        results = [
            block for message in turn for block in
            (message["content"] if isinstance(message["content"], list) else [])
            if block.get("type") == "tool_result"
        ]
        step = len(results)
        own = tool("get_memory", {"reference": ref})
        foreign = tool("get_memory", {"reference": forwarded})

        if name in ("UNCONFIGURED", "TIMEOUT", "POLICYMISS"):
            assert ref is None, (name, ref)
            return None
        if name == "POLICYHIT":
            assert ref, (name, ref)
            return None
        if name == "HOLD":
            if step == 0:
                assert len(set(refs)) >= 2, f"expected cross-domain recall (coding + business), got {refs}"
                self.pause("hold")
                return own
            return None
        if name == "SECOND":
            assert ref, "second process received no recall"
            actions = [own]
        elif name == "LATE":
            actions = [foreign]
        elif name == "CORRECT":
            assert ref, "correction turn received no recall"
            fb = tool("submit_memory_feedback", {
                "reference": ref, "kind": "correction",
                "reason": "团队规范更新：Go 接口首参用 context 并必须设置超时",
                "evidence": "2026 Go 规范修订：所有 RPC 必须传递带超时的 context.Context。",
                "idempotency_key": "retry-key-1",
            })
            actions = [own, fb, fb]
        elif name == "FOLLOWUP":
            assert ref, "contextual recall was not injected on the user prompt"
            actions = [("Read", {"file_path": str(self.doc_file)}), *[tool("get_memory", {"reference": r}) for r in dict.fromkeys(refs)]]
        elif name == "FORMED":
            assert ref, "newly formed memory did not arrive"
            actions = [own]
        elif name == "RESUME":
            assert ref and ref != forwarded, "resume must issue a fresh task reference"
            actions = [foreign, own]
        elif name == "ONLINE":
            assert ref, "recall did not work before disconnect"
            actions = [own]
        elif name == "OFFLINE":
            assert ref is None, name
            actions = [foreign]
        else:
            raise AssertionError(name)
        return actions[step] if step < len(actions) else None


def mock_server(script):
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if self.path == "/v1/embeddings":
                inputs = body.get("input", [])
                if isinstance(inputs, str):
                    inputs = [inputs]
                data = [{"index": i, "embedding": topic_vector(t)} for i, t in enumerate(inputs)]
                self.send(200, "application/json", json.dumps({"data": data}))
                return

            if self.path == "/v1/systemone":
                instr = body["questions"]["decision"]["instructions"]
                state = body.get("state", {})
                state_str = json.dumps(state, ensure_ascii=False)
                prob = 0.90
                if "导出图标" in state_str and "systemic" not in instr:
                    prob = 0.35
                elif "闲聊天气" in state_str:
                    prob = 0.05
                self.send(200, "application/json", json.dumps({
                    "model": body["model"],
                    "answers": {"decision": {"type": "noul", "noul": prob}},
                }))
                return

            if self.path == "/v1/chat/completions":
                sys_msg = body["messages"][0]["content"]
                user_msg = body["messages"][1]["content"]
                if "standalone semantic search query" in sys_msg:
                    context = json.loads(user_msg)
                    query = context["user_prompt"]
                    if "CASE_FOLLOWUP" in query and context["previous_assistant_answer"] == "MNEVO_TEST_DONE":
                        query = "Go 接口 context 规范与超时设置"
                    content = json.dumps({"query": query, "keywords": []}, ensure_ascii=False)
                elif "should_revise" in sys_msg:
                    content = json.dumps({
                        "should_revise": True,
                        "reason": "已核实规范补充超时约束",
                        "summary": "Go 接口首参统一传 context.Context 且必须设置超时",
                        "details": "所有 RPC 调用必须传递带超时的 context.Context。",
                        "conditions": "Go 后端代码开发",
                        "tags": ["go", "编程规范", "context"],
                    }, ensure_ascii=False)
                elif "target_type" in sys_msg:
                    content = json.dumps({
                        "target_type": "setting",
                        "target_key": "recall.jev_threshold",
                        "after_value": 0.55,
                        "diagnosis": "3 次误注入的 jev_p 均为 0.35，提高 recall.jev_threshold 可过滤此类误注入。",
                        "impact": "过滤边界低分误注入",
                        "risk": "轻微降低召回覆盖率",
                    }, ensure_ascii=False)
                elif "周报" in user_msg:
                    content = json.dumps({"memories": [{
                        "type": "preference",
                        "summary": "周报开头先给三条核心结论",
                        "tags": ["周报", "结论"],
                        "conditions": "撰写周报与汇报时",
                        "details": "先写结论再展开数据明细。",
                    }]}, ensure_ascii=False)
                elif "导出报表" in user_msg:
                    content = json.dumps({"memories": [{
                        "type": "procedure",
                        "summary": "导出报表前必须先核对源表",
                        "tags": ["导出", "源表"],
                        "conditions": "仅用于财务报表导出",
                    }]}, ensure_ascii=False)
                elif "退款" in user_msg or "context" in user_msg.lower():
                    content = json.dumps({"memories": [
                        {
                            "type": "guideline",
                            "summary": "Go 接口方法首参统一使用 context.Context",
                            "tags": ["go", "编程规范"],
                            "conditions": "Go 后端代码开发",
                        },
                        {
                            "type": "fact",
                            "summary": "部分退款时优惠券按实付金额比例分摊扣减",
                            "tags": ["支付", "退款"],
                            "conditions": "涉及订单退款结算逻辑时",
                            "details": "退款口径以结算规则第 3 章为准。",
                        },
                    ]}, ensure_ascii=False)
                else:
                    content = json.dumps({"memories": []})
                self.send(200, "application/json", json.dumps({
                    "choices": [{"finish_reason": "stop", "message": {"content": content}}],
                }))
                return

            if self.path.startswith("/v1/messages/count_tokens"):
                self.send(200, "application/json", '{"input_tokens": 20}')
                return

            try:
                action = script.next_action(body)
            except Exception as error:
                script.events.put(f"mock model assertion: {error!r}")
                self.send(400, "application/json", json.dumps(
                    {"type": "error", "error": {"type": "invalid_request_error", "message": str(error)}}))
                return
            id_ = uuid.uuid4().hex
            if action:
                name, args = action
                block = {"type": "tool_use", "id": "toolu_" + id_, "name": name, "input": {}}
                delta = {"type": "input_json_delta", "partial_json": json.dumps(args, ensure_ascii=False)}
                stop = "tool_use"
            else:
                block = {"type": "text", "text": ""}
                delta = {"type": "text_delta", "text": "MNEVO_TEST_DONE"}
                stop = "end_turn"
            frames = [
                {"type": "message_start", "message": {
                    "id": "msg_" + id_, "type": "message", "role": "assistant",
                    "model": body["model"], "content": [], "stop_reason": None,
                    "stop_sequence": None, "usage": {"input_tokens": 20, "output_tokens": 0}}},
                {"type": "content_block_start", "index": 0, "content_block": block},
                {"type": "content_block_delta", "index": 0, "delta": delta},
                {"type": "content_block_stop", "index": 0},
                {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                 "usage": {"output_tokens": 10}},
                {"type": "message_stop"},
            ]
            self.send(200, "text/event-stream", "".join(
                f"event: {item['type']}\ndata: {json.dumps(item, ensure_ascii=False)}\n\n" for item in frames
            ))

        def send(self, status, content_type, body):
            payload = body.encode()
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            try:
                self.wfile.write(payload)
            except (BrokenPipeError, ConnectionResetError):
                pass

    return http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)


@contextlib.contextmanager
def service(binary, home, delay=None):
    home.mkdir(mode=0o700, exist_ok=True)
    args = [str(binary), "-home", str(home), "serve"]
    if delay:
        args += ["-test-recall-delay", delay]
    with (home / "service.log").open("w") as log, subprocess.Popen(args, env=clean_env(), stdout=log, stderr=log) as proc:
        try:
            for _ in range(100):
                if (home / "mnevo.sock").is_socket():
                    break
                if proc.poll() is not None:
                    raise RuntimeError((home / "service.log").read_text())
                time.sleep(0.05)
            else:
                raise TimeoutError("service startup")
            yield
        finally:
            if proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=5)


def admin(binary, home, *args, no_autostart=False):
    extra = {"MNEVO_NO_AUTOSTART": "1"} if no_autostart else None
    out = subprocess.check_output([str(binary), "-home", str(home), *args], env=clean_env(extra), text=True)
    return json.loads(out)


class Claude:
    """Runs native `claude` directly using config written by `mnevo setup`."""

    def __init__(self, stack, claude_dir, work, url, name, persist=False, resume=None, no_autostart=False):
        extra = {
            "ANTHROPIC_BASE_URL": url,
            "ANTHROPIC_API_KEY": "local-scripted-test",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
            "DISABLE_NON_ESSENTIAL_MODEL_CALLS": "1",
            "NO_PROXY": "127.0.0.1",
            "no_proxy": "127.0.0.1",
            "CLAUDE_CONFIG_DIR": str(claude_dir),
        }
        if no_autostart:
            extra["MNEVO_NO_AUTOSTART"] = "1"
        self.name = name
        self.events = queue.Queue()
        self.output = work / f"{name}.jsonl"
        stderr = stack.enter_context((work / f"{name}.stderr").open("w"))
        cmd = [
            "claude",
            *(["--resume", resume] if resume else []),
            "-p", "--tools", "Read", "--permission-mode", "dontAsk",
            "--allowedTools", "Read,mcp__mnevo__get_memory,mcp__mnevo__submit_memory_feedback",
            "--input-format", "stream-json", "--output-format", "stream-json",
            "--verbose", "--include-hook-events",
            *([] if persist else ["--no-session-persistence"]),
        ]
        self.proc = subprocess.Popen(
            cmd, cwd=work, env=clean_env(extra),
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, text=True,
        )
        self.thread = threading.Thread(target=self.collect, daemon=True)
        self.thread.start()
        stack.callback(self.close)

    def collect(self):
        with self.output.open("w") as log:
            for line in self.proc.stdout:
                log.write(line)
                log.flush()
                self.events.put(json.loads(line))
        self.events.put(None)

    def close(self):
        if self.proc.stdin and not self.proc.stdin.closed:
            self.proc.stdin.close()
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.terminate()
            self.proc.wait(timeout=5)
        self.thread.join(timeout=5)
        if self.proc.stdout and not self.proc.stdout.closed:
            self.proc.stdout.close()

    def prompt(self, text):
        self.proc.stdin.write(json.dumps({
            "type": "user", "message": {"role": "user", "content": text},
        }, ensure_ascii=False) + "\n")
        self.proc.stdin.flush()

    def finish(self):
        events = []
        while True:
            event = self.events.get(timeout=TIMEOUT)
            if event is None:
                raise RuntimeError(f"{self.name} exited; see {self.output}")
            events.append(event)
            if event["type"] == "result":
                assert not event.get("is_error") and event.get("result") == "MNEVO_TEST_DONE", event
                return events


def reference(events, hook="UserPromptSubmit"):
    for event in events:
        if event.get("subtype") != "hook_response" or event.get("hook_event") != hook:
            continue
        if match := REFERENCE.search(event.get("stdout", "")):
            return match.group(1)
    raise AssertionError(f"no reference in {hook} output")


def memory_results(events):
    results = []
    for event in events:
        if event.get("type") != "user":
            continue
        for block in event.get("message", {}).get("content", []):
            if not isinstance(block, dict) or block.get("type") != "tool_result":
                continue
            content = block.get("content", [])
            parts = [x.get("text", "") for x in content if isinstance(x, dict)] if isinstance(content, list) else [content]
            for part in parts:
                try:
                    data = json.loads(part)
                except (TypeError, ValueError):
                    continue
                if isinstance(data, dict) and "status" in data:
                    results.append(data)
    return results


def status(results, *expected):
    assert [result["status"] for result in results] == list(expected), results


def configure_models(binary, home, url):
    admin(binary, home, "model", "set", "embedding", json.dumps({
        "enabled": True, "url": f"{url}/v1", "model": "test-embed", "api_key": "k",
    }))
    admin(binary, home, "model", "set", "jev", json.dumps({
        "enabled": True, "url": f"{url}/v1/systemone", "model": "test-jev", "api_key": "k",
    }))
    admin(binary, home, "model", "set", "llm", json.dumps({
        "enabled": True, "url": f"{url}/v1", "model": "test-llm", "api_key": "k",
    }))
    for role in ("embedding", "jev", "llm"):
        assert admin(binary, home, "model", "test", role)["status"] == "ok"


def run(binary, directory, script, url, doc_file):
    home = directory / "home"
    claude_dir = directory / "claude-config"
    claude_dir.mkdir(mode=0o700)
    # Pre-populate user settings & another MCP server to verify `mnevo setup` / `uninstall` preserve them!
    (claude_dir / "settings.json").write_text(json.dumps({"cleanupPeriodDays": 30}), encoding="utf-8")
    (claude_dir / ".claude.json").write_text(json.dumps({
        "mcpServers": {"keep_me": {"type": "stdio", "command": "true"}},
    }), encoding="utf-8")

    # 0. One-time native setup + verify daemon starts lazily on demand!
    setup_res = admin(binary, home, "setup", "-claude-dir", str(claude_dir))
    assert setup_res["status"] == "ok", setup_res
    assert admin(binary, home, "status")["status"] == "stopped"

    try:
        with contextlib.ExitStack() as stack:
            # 1. Native Claude auto-starts background daemon AND injects nothing when unconfigured.
            dummy = admin(binary, home, "memory", "add", json.dumps({
                "type": "guideline", "summary": "未配模型不应注入的记忆", "tags": ["go"], "conditions": "Go",
            }, ensure_ascii=False))
            health = admin(binary, home, "status")
            assert health["status"] == "ok" and health["pid"] > 0 and health["web_url"].startswith("http://127.0.0.1:"), health
            # Verify TCP transport isolation on auto-started Web server: /v1/health MUST 404 over TCP!
            try:
                urllib.request.urlopen(health["web_url"] + "/v1/health", timeout=3)
                raise AssertionError("TCP /v1/health unexpectedly succeeded")
            except urllib.error.HTTPError as exc:
                assert exc.code == 404, exc.code

            first = Claude(stack, claude_dir, directory, url, "first")
            first.prompt("CASE_UNCONFIGURED 写 Go 代码规范")
            first.finish()
            aud = admin(binary, home, "audit")["audit"]
            assert any(r.get("model_outcome") == "models_not_configured" for r in aud), aud
            admin(binary, home, "memory", "delete", dummy["id"])

            # 2. Hot-configure Embedding + Jev + LLM without restarting daemon, then smart-import!
            configure_models(binary, home, url)
            imported = admin(binary, home, "import", str(doc_file))
            assert imported["status"] == "ok", imported

            # 3. Cross-domain recall + concurrent session isolation + Stop revocation in SQLite!
            second = Claude(stack, claude_dir, directory, url, "second", persist=True)
            first.prompt("CASE_HOLD 用 Go 实现支付退款接口")
            script.wait("hold")

            second.prompt("CASE_SECOND 用 Go 实现支付退款接口")
            second_turn = second.finish()
            second_ref = reference(second_turn)
            status(memory_results(second_turn), "ok")

            # first is still mid-turn when second's Stop revoked ONLY second's references!
            script.gates["hold"].set()
            initial = first.finish()
            first_ref = reference(initial)
            assert first_ref != second_ref
            status(memory_results(initial), "ok")

            # Now first's Stop has fired -> first_ref is revoked in SQLite!
            first.prompt(f"CASE_LATE {first_ref} 验证上一轮引用在 Stop 后已失效")
            status(memory_results(first.finish()), "unavailable")

            # 4. Feedback revises the memory to v2. A contextual user follow-up recalls it before Read.
            first.prompt("CASE_CORRECT 请检查 Go 接口 context 规范")
            corr = memory_results(first.finish())
            status(corr, "ok", "accepted", "accepted")
            assert corr[1]["receipt"] == corr[2]["receipt"]
            for _ in range(50):
                entries = admin(binary, home, "audit")["audit"]
                if any(row["result"] == "applied" for row in entries):
                    break
                time.sleep(0.1)
            else:
                raise TimeoutError("LLM feedback revision not applied")
            second.prompt("CASE_FOLLOWUP 那这一处呢？请先读取本地文件再核对。")
            followup = second.finish()
            reference(followup)
            assert not any(event.get("hook_event") == "PostToolBatch" for event in followup)
            revised = memory_results(followup)
            assert revised and all(r["status"] == "ok" for r in revised), revised
            assert any(r["version"] == 2 and "超时" in r["summary"] for r in revised), revised

            # 5. Natural-language 记住：... + native `claude --resume`!
            first.prompt("记住：以后写周报开头先给三条核心结论")
            ack = first.finish()
            assert any("Mnevo 已收到" in e.get("stdout", "") for e in ack), ack
            for _ in range(50):
                mems = admin(binary, home, "memory", "list", "核心结论").get("memories", [])
                if mems and mems[0]["embedded"]:
                    break
                time.sleep(0.1)
            else:
                raise TimeoutError("natural-language memory not formed & embedded")

            with contextlib.ExitStack() as rs:
                reader = Claude(rs, claude_dir, directory, url, "reader", persist=True)
                reader.prompt("CASE_FORMED 帮我写本周周报结论")
                turn = reader.finish()
                old_ref = reference(turn)
                status(memory_results(turn), "ok")
                entries = admin(binary, home, "audit")["audit"]
                old_session = next(r["session_id"] for r in entries if r.get("reference") == old_ref)
            with contextlib.ExitStack() as rs:
                resumed = Claude(rs, claude_dir, directory, url, "resumed", persist=True, resume=old_session)
                resumed.prompt(f"CASE_RESUME {old_ref} 帮我写本周周报结论")
                turn = resumed.finish()
                status(memory_results(turn), "unavailable", "ok")
                assert reference(turn) != old_ref

            # 6. Meta-learning: 3 false_injection observations -> Jev -> LLM diagnosis -> strategy.json approval & rollback!
            export_doc = directory / "export.md"
            export_doc.write_text("导出报表前必须先核对源表。", encoding="utf-8")
            assert admin(binary, home, "import", str(export_doc))["status"] == "ok"
            cfg = admin(binary, home, "strategy", "set", "recall.jev_threshold",
                        '{"value":0.25,"learn":true,"apply":"approval","bounds":[0.05,0.90]}')
            base_ver = cfg["policy"]["version"]
            assert (home / "strategy.json").is_file()

            def ask_export(expected):
                second.prompt(f"CASE_POLICY{expected} 导出图标")
                second.finish()
                entries = admin(binary, home, "audit")["audit"]
                return next(r for r in entries if r["operation"] == "recall")

            for _ in range(3):
                dec = ask_export("HIT")
                admin(binary, home, "strategy", "observe", "false_injection", dec["task_id"], "导出图标与报表导出无关")
            for _ in range(60):
                props = admin(binary, home, "strategy", "proposals").get("proposals", [])
                if props:
                    break
                time.sleep(0.1)
            else:
                raise TimeoutError("meta-learning proposal not generated")
            assert props[0]["status"] == "pending" and props[0]["after"] == 0.55, props
            ask_export("HIT")
            admin(binary, home, "strategy", "approve", props[0]["id"])
            ask_export("MISS")
            admin(binary, home, "strategy", "rollback", str(base_ver))
            ask_export("HIT")
    finally:
        # Cleanly stop the auto-started background daemon via `mnevo stop`.
        assert admin(binary, home, "stop")["status"] == "stopped"
        assert admin(binary, home, "status")["status"] == "stopped"

    # 7. Recall timeout fails open without blocking Claude Code.
    slow = directory / "slow"
    slow_cfg = directory / "claude-slow"
    admin(binary, slow, "setup", "-claude-dir", str(slow_cfg))
    with service(binary, slow, "2s"), contextlib.ExitStack() as stack:
        configure_models(binary, slow, url)
        client = Claude(stack, slow_cfg, directory, url, "timeout", no_autostart=True)
        client.prompt("CASE_TIMEOUT Go 规范")
        client.finish()
        entries = admin(binary, slow, "audit", no_autostart=True)["audit"]
        assert any(row["result"] == "timeout" for row in entries), entries

    # 8. Daemon disconnect fails open cleanly (when auto-start disabled via MNEVO_NO_AUTOSTART=1).
    disc = directory / "disconnect"
    disc_cfg = directory / "claude-disc"
    admin(binary, disc, "setup", "-claude-dir", str(disc_cfg))
    with contextlib.ExitStack() as stack:
        with service(binary, disc):
            configure_models(binary, disc, url)
            assert admin(binary, disc, "import", str(doc_file), no_autostart=True)["status"] == "ok"
            client = Claude(stack, disc_cfg, directory, url, "offline", no_autostart=True)
            client.prompt("CASE_ONLINE Go 接口规范")
            online = client.finish()
            status(memory_results(online), "ok")
            ref = reference(online)
        client.prompt(f"CASE_OFFLINE {ref} Go 接口规范")
        status(memory_results(client.finish()), "unavailable")

    # 9. Uninstall cleanly removes Mnevo entries while preserving user settings & other MCP servers.
    assert admin(binary, home, "uninstall", "-claude-dir", str(claude_dir))["status"] == "ok"
    settings_after = json.loads((claude_dir / "settings.json").read_text(encoding="utf-8"))
    assert settings_after.get("cleanupPeriodDays") == 30 and "mnevo" not in json.dumps(settings_after), settings_after
    mcp_after = json.loads((claude_dir / ".claude.json").read_text(encoding="utf-8"))
    assert "keep_me" in mcp_after.get("mcpServers", {}) and "mnevo" not in mcp_after.get("mcpServers", {}), mcp_after


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    config = parser.parse_args()
    directory = Path(tempfile.mkdtemp(prefix="mnevo-int-"))
    print(f"Integration artifacts: {directory}", flush=True)
    doc_file = directory / "engineering-and-business.md"
    doc_file.write_text(
        "1. Go 接口方法首参统一使用 context.Context。\n"
        "2. 部分退款时优惠券按实付金额比例分摊扣减。\n",
        encoding="utf-8",
    )
    script = Script(doc_file)
    with mock_server(script) as api:
        thread = threading.Thread(target=api.serve_forever, daemon=True)
        thread.start()
        try:
            run(config.binary.resolve(), directory, script, f"http://127.0.0.1:{api.server_port}", doc_file)
        finally:
            api.shutdown()
            thread.join()
    print("PASS: native Claude Code + mnevo setup + unified HTTP daemon + Dual-System E2E")


if __name__ == "__main__":
    main()
