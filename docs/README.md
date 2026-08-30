# 文档索引

本文档目录同时包含“当前实现说明”和“变更复盘/设计稿”。阅读运行方式与行为时，
以根目录 [`README.md`](../README.md) 和下面标记为“当前”的文档为准；带有“复盘”
字样的文档保留历史背景，不应当被当作独立的 API 参考。

## 当前实现

- [`conversation-and-trace.md`](conversation-and-trace.md)：Conversation、Run、Trace 的存储格式、写入时机和清理边界。
- [`cli-message-flow.md`](cli-message-flow.md)：CLI 输入到 AgentLoop、Runner、Provider 和输出的完整链路。
- [`function-calling-flow.md`](function-calling-flow.md)：工具注册、模型调用、工具执行和结果回传。
- [`tools-and-sandbox.md`](tools-and-sandbox.md)：文件工具、联网工具和 Docker 沙盒的权限边界。
- [`skill-execution-path-review.md`](skill-execution-path-review.md)：Skill 渐进式加载和 Path 评审实现。
- [`resume-project-description.md`](resume-project-description.md)：Yomi 的简历项目经历、技术亮点和面试讲解主线。
- [`context-and-memory-plan.md`](context-and-memory-plan.md)：Context 管理、长会话压缩和长期 Memory 的建设计划。
- [`context-layered-compaction-design.md`](context-layered-compaction-design.md)：将工具结果预算、噪声删除、API 微压缩、归档摘要和全量压缩落到 Yomi 的分层设计稿。

## 设计与变更复盘

以下文档记录实现过程中的方案、问题和取舍。它们中的“改动前”“未来”“规划”
描述的是对应文档产生时的状态，若与代码冲突，以代码为准：

- [`user-memory-v1-design.md`](user-memory-v1-design.md)：Yomi 用户级长期记忆第一版设计稿，包含数据模型、读写路径、存储、隐私和验收标准。
- `session-and-streaming.md`
- `reasoning-and-tool-events.md`
- `tools-integration-flow.md`
- `harness/agent-status-line.md`
- `harness/ask-user-question-flow.md`
- `harness/disconnect-cancellation.md`
- `harness/streaming-passthrough-and-sidecar-persistence.md`

## Skill Review 设计材料

- [`skill-review-plan.md`](skill-review-plan.md)：评审数据模型与实现计划。
- [`skill-review-ui-redesign.md`](skill-review-ui-redesign.md)：工作台界面设计稿。

实际命令以 `go run ./cmd/skill-review -h` 为准。当前已支持 `-serve`、Skill 编辑、
Path 缓存/生成，以及 Markdown/JSON 报告输出。

## 生成项目地图

`cmd/overview` 会实时扫描当前 workspace 的 `skills/` 和 `docs/`，适合快速查看实际
文件和 Skill 状态：

```bash
go run ./cmd/overview -addr 127.0.0.1:8091 -workspace .
```
