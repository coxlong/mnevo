# Mnevo · 忆循

Mnevo 是面向个人 AI Agent 的本机长期记忆服务。执行 `mnevo setup` 后，直接使用原生 `claude`。每次提交消息时，Mnevo 可以结合上一轮助手回答检索记忆，并自动注入相关摘要。对话抽取、文档导入和显式反馈会持续补充或更新记忆。

Mnevo 使用 Go、GORM 和 SQLite（WAL 模式），服务数据和配置位于 `~/.mnevo/`。文档入口包括 [当前架构与流程](docs/CURRENT_STATE.md)、[进程与模型协议](docs/PROTOCOL.md)、[Claude Code 集成验证](docs/INTEGRATION.md) 和 [产品需求](docs/PRD.md)。

---

## 目录与配置布局

默认目录为 `~/.mnevo/`（可用 `-home /path/to/dir` 或 `MNEVO_HOME` 覆盖，权限强制 `0700`）：

```text
~/.mnevo/
├── config.json          # 0600：Embedding / Jev / LLM 连接与 API Key（Web/CLI 热生效）
├── strategy.json        # 0600：当前生效的策略参数、元学习参数与可进化 Prompt（明文可读可改）
├── strategy.history/    # 不可变历史快照（v1.json, v2.json ... 支持一键回滚与审计溯源）
├── memory.sqlite        # SQLite (WAL)：记忆、版本、向量 BLOB、会话轮次、引用、队列、诊断/提案、审计
├── admin.key            # 0600：本机 CLI 与 Web 管理页鉴权密钥
├── mnevo.sock           # 0600：Unix 域套接字（HTTP/1.1 REST API + Linux SO_PEERCRED 校验）
├── service.lock         # 0600：单实例排他锁（flock）
└── service.log          # 0600：后台常驻服务运行日志
```

---

## 安装与快速开始

需要 Linux / macOS、Go 1.27.3+（仅编译时需要）以及已安装的 Claude Code（`claude`）。

```sh
cd mnevo
make install
# 或：CGO_ENABLED=0 go build -o ~/.local/bin/mnevo .
```

`make build` 使用随源码提交的 Go 模板和 CSS 产物。修改前端后，可用 templ v0.3.1118 与 Tailwind CSS v4 CLI 运行 `make frontend` 重新生成。工具未加入 PATH 时，可传入 `TEMPL=/path/to/templ` 和 `TAILWIND=/path/to/tailwindcss`。

1. **一次性接入 Claude Code**（幂等合并写入 `~/.claude/settings.json` 与 `~/.claude.json`，保留已有配置）：
   ```sh
   mnevo setup
   ```
   如需卸载接入配置：`mnevo uninstall`。

2. **直接正常使用原生 `claude`**（无需包装命令，也无需手动先起服务）：
   ```sh
   claude
   # 恢复已有会话同样直接用原生参数：
   claude --resume <SESSION_UUID>
   ```
   当 `claude` 触发 Hook/MCP 或执行 CLI 管理命令时，若后台守护进程尚未运行，Mnevo 会自动以独立后台会话拉起 `mnevo serve -web 127.0.0.1:8765 -web-fallback`（若 8765 被占用则自动回退到随机回环端口）。
   - 查看运行状态与当前 Web 管理页地址：`mnevo status`
   - 优雅停止后台服务：`mnevo stop`
   - 如需由 `systemd`/`launchd` 托管而非自动拉起，可设置环境变量 `MNEVO_NO_AUTOSTART=1` 并显式运行 `mnevo serve -web 127.0.0.1:8765`。

3. **在本地 Web 管理页配置模型（实时热生效，无需重启）**：
   - 运行 `mnevo status` 查看 `web_url`（默认 `http://127.0.0.1:8765`），浏览器打开后使用用户名 `mnevo` 与密钥（`cat ~/.mnevo/admin.key`）登录品牌化登录页；会话有效期 12 小时，支持退出登录。
   - 打开 **「模型配置」** Tab，配置并启用：
     - **Embedding 模型**（OpenAI 兼容 `/v1/embeddings`，支持 OpenRouter / OpenAI / 本机 Ollama·vLLM）
     - **System One / Jev 模型**负责判断续问是否值得检索、候选是否适用，以及后台抽取、导入、反馈和策略诊断的门控
     - **通用 LLM**使用 OpenAI 兼容 Chat Completions JSON 模式，负责续问查询改写、记忆提炼、反馈修订和策略诊断
   - 可点击 **「测试连接」** 即时校验；也可在环境变量初始化（`MNEVO_EMBED_*`、`MNEVO_JEV_*`、`MNEVO_LLM_*`）。
   - 保持同一模型地址时可留空沿用已保存密钥。更换地址时请填写新地址的密钥，或选择清除原密钥。
   - **召回需要 Embedding 和 Jev**。聊天模型可选，查询改写出错或超时时会用已有上下文继续检索。同步召回有时间上限，Claude Code 可以继续当前任务。

---

## 核心使用方式

### 1. 形成与导入记忆（直接生效 + 自动向量化）
- **对话自动抽取**：每轮对话结束（`Stop` / `SessionEnd`），Mnevo 在 SQLite 中原子撤销本轮短期引用，并按增量游标读取 Claude Code 的 `transcript_path`，后台先经 **Jev** 过滤闲聊，再由 **LLM** 抽取有长期价值的偏好、规范、业务事实与排障经验，直接生效并生成向量。
- **自然语言显式记录**：在 Claude Code 直接输入一行 `记住：<任意自然语言>`（也兼容结构化 `记住：{JSON}`），毫秒级返回回执并由后台 LLM 提炼入库。
- **Web / CLI 智能导入**：在 Web 首页粘贴 Markdown/笔记，或执行：
  ```sh
  mnevo import ./notes.md
  ```
- **切换 Embedding 模型自动补齐**：在 Web 更换 Embedding 模型后，后台 `EmbedNext` 自动为所有存量记忆补齐向量索引。

### 2. 跨领域语义召回、查证与反馈
- 用户提交消息时，Mnevo 按需读取上一轮助手回答。Jev 判断续问是否需要检索，聊天模型可将其改写为独立查询。Embedding 向量化后，Go 按相似度、记忆强度和时间衰减排序，Jev 筛选通过的记忆会自动注入。
- Agent 可调用 MCP 工具 `get_memory` 查证细节与出处，通过 `submit_memory_feedback` 提交 `helpful` / `not_applicable` / `outdated` / `correction` 反馈（修订反馈先过 Jev 门禁再由 LLM 核验发布新版本并重新向量化）。

### 3. 文件化策略与元学习（`~/.mnevo/strategy.json`）
- 可直接在 Web **「策略与提案」** 页或编辑 `~/.mnevo/strategy.json`（自动热加载）调整召回、衰减、Jev 门控、元学习阈值及策略提示词。
- 当同策略版本下累积足够独立任务的已核实诊断证据（默认 `meta.threshold = 3`），后台先经 **Jev** 判断是否属系统性偏差，再由 **LLM** 结合审计中的 `vec` / `score` / `jev_p` 分布生成带根因分析的调参或 Prompt 修订提案。
- 批准后自动归档 `~/.mnevo/strategy.history/vN.json` 并原子更新 `strategy.json`；发现副作用可一键回滚（发布新版本恢复历史值，保留现行学习边界）。

---

## 常用 CLI 命令

```sh
mnevo version
mnevo setup                     # 一次性接入 Claude Code
mnevo uninstall                 # 从 Claude Code 移除 Mnevo 配置
mnevo status                    # 查看后台服务 PID、Web 地址、模型就绪态与向量化进度
mnevo stop                      # 优雅停止后台服务

mnevo model show
mnevo model set embedding '{"enabled":true,"url":"http://127.0.0.1:11434/v1","model":"nomic-embed-text"}'
mnevo model test embedding
mnevo web set https://mnevo.example.com   # 可选：配置公网/反代入口（支持子路径，热生效）
mnevo web clear                           # 恢复仅允许本机回环访问

mnevo import ./docs/guide.md
mnevo memory list '关键词'
mnevo memory show <MEMORY_ID>
mnevo memory add '{"type":"guideline","summary":"Go 接口首参统一传 context.Context","tags":["go","编程规范"],"conditions":"Go 后端开发"}'
mnevo memory disable <MEMORY_ID>
mnevo memory enable <MEMORY_ID>
mnevo memory delete <MEMORY_ID>

mnevo strategy show
mnevo strategy history
mnevo strategy set recall.jev_threshold '{"value":0.25,"learn":true,"apply":"approval","bounds":[0.05,0.90]}'
mnevo strategy set-prompt jev_recall '{"value":"...","learn":true,"apply":"approval"}'
mnevo strategy observe false_injection <TASK_ID> '与当前任务无关'
mnevo strategy proposals
mnevo strategy approve <PROPOSAL_ID>
mnevo strategy rollback <VERSION>

mnevo audit [BEFORE_ID]
```

---

## 复验

```sh
make check
make build
python3 tests/claude_integration.py dist/mnevo
python3 tests/claude_tty.py dist/mnevo
```

`tests/claude_integration.py` 与 `tests/claude_tty.py` 直接驱动原生 Claude Code 进程对接本地回环模拟的 Embedding + Jev + LLM + Messages API，覆盖 `mnevo setup` / `uninstall`、按需自动拉起后台服务与 `status` / `stop`、TCP 与 Unix Socket 传输隔离、未配置模型不注入、热配置模型、智能导入、跨领域召回、并发会话与 `Stop` SQLite 引用撤销、反馈修订、基于上一轮回答的用户续问召回、自然语言 `记住：`、`--resume`、元学习审批回滚、超时与断开容错。若 `/tmp` 空间紧张，请在构建命令前设置 `TMPDIR=/path/to/writable/dir`。
