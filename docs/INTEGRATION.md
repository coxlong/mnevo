# Claude Code 集成与验证

Mnevo 通过 `mnevo setup` 在 Claude Code 中登记 Hook 和 MCP。Claude Code 直接运行原生 `claude`，需要时由适配器自动启动 Daemon。架构与模型分工见 [当前架构与流程](CURRENT_STATE.md)，配置路径、请求与响应格式见 [协议规范](PROTOCOL.md)。

## 验证当前行为

| 场景 | 预期 |
| --- | --- |
| 首条普通消息 | 用用户原话检索。匹配时收到摘要、适用条件和记忆引用。 |
| 有上一轮回答的简短续问 | 从 transcript 读取上一轮助手文本。Jev 与已配置的聊天模型可利用其背景，使检索找到只靠续问字面难以命中的记忆。 |
| 确认语与改写失败 | Jev 可以跳过确认语。改写失败时沿用当前消息和上一轮回答。 |
| 工具执行 | Read 等工具的结果不会触发 Mnevo 召回。重新运行 `mnevo setup` 清理旧版的 Mnevo `PostToolBatch` 配置，保留用户自己的 Hook。 |
| 引用生命周期 | 本轮可通过 MCP 回查或提交反馈。`Stop`、`StopFailure`、`SessionEnd` 或同 Session 的下一轮使旧引用失效。 |
| 模型与服务容错 | Embedding 或 Jev 不可用时，Claude Code 可以继续任务。已有 transcript 的恢复会话可在下一次用户输入时重新召回。 |

引用是同 UID 本机 IPC 范围内的短期凭证。Daemon 依据引用、撤销状态、有效期和记忆当前版本判断是否可用。MCP 请求通过引用定位交付记录。

## 运行测试

```sh
make test
make build
python3 tests/claude_integration.py dist/mnevo
python3 tests/claude_tty.py dist/mnevo
```

Go 测试覆盖 Hook 转发、确认语跳过、查询改写失败回退、原文关键词校验以及 SQLite 轮次和引用。集成脚本运行原生 Claude Code，通过本地回环模拟 Messages、Embedding、Jev 和 LLM 接口。`claude_integration.py` 覆盖续问利用上一轮回答召回修订后的记忆，并验证后续工具批次没有额外注入。`claude_tty.py` 用于验证交互终端中的 Hook 与 MCP 行为。测试使用临时配置和数据目录。

早期的验收环境和结果见 [v0.1 历史验收记录](verification/v0.1.md)。
