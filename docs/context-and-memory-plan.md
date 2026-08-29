# Context 与 Memory 建设计划

本文档是后续实现计划，不改变当前行为说明。当前系统已经有 `SessionStore`、`Trace`、单次 Run 内的工具上下文和 token budget；本计划在此基础上补齐长会话上下文管理与跨会话记忆。

## 目标与边界

目标是让 Agent 在上下文窗口有限、会话持续很久、工具返回很大或进程重启后，仍然能够：

- 保留当前任务真正需要的历史；
- 找回与当前问题相关的旧事实和旧事件；
- 控制 token、延迟和成本；
- 明确区分对话主线、运行轨迹、临时状态和长期记忆；
- 允许用户纠正、删除和查看记忆。

核心边界：

```text
Conversation  = 面向后续对话的主线历史
Trace         = 面向排障、审计和 UI 的完整运行轨迹
WorkingContext= 本轮实际发送给模型的消息集合
Memory        = 跨轮或跨会话、未来值得复用的事实/事件/规则
Artifact      = 大型工具结果或文件内容的外部引用
```

不把 Trace 全量回放给模型，也不把每一句对话自动写成长期记忆。

## 总体流程

```text
Inbound
  -> Load conversation metadata
  -> Estimate token usage
  -> Compact old history when necessary
  -> Retrieve relevant memories
  -> Add current task state and artifact references
  -> Build WorkingContext
  -> Runner tool loop
  -> Persist conversation and trace
  -> Extract memory candidates
  -> Apply write policy
  -> Update summary/index
```

## 里程碑

### M1：Context Builder 与预算控制

- [x] 新增 `ContextManager`，Loop 不再直接负责历史拼装。
- [x] 定义基础 `ContextResult` 数据结构。
- [x] 估算 system、历史和当前输入的 token 预算。
- [x] 配置 context limit、保留最近消息数和压缩触发阈值。
- [ ] 在发送模型前记录最终上下文的来源、消息数和估算 token 数。
- [x] 保证 system prompt 前缀稳定，动态内容追加在上下文末尾。
- [x] 预算不足时返回可观测的 `context_budget_exceeded`，不能静默截断当前用户输入。

验收：固定输入下能断言最终 messages 顺序；超过阈值时仍保留当前请求、最近对话和未完成任务。

### M2：Rolling Summary 与 Conversation Compaction

- [x] 为 Session 增加摘要元数据：summary、覆盖消息范围、版本、更新时间。
- [x] 实现按 token 阈值触发的 compaction，而不是按固定轮数硬截断。
- [ ] 摘要至少包含：事实、用户约束、已做决定、未完成任务、关键文件/资源引用。
- [ ] 保留最近若干轮原文，较早历史替换为一条结构化 summary message。
- [ ] compaction 具备幂等性；重复执行不会不断膨胀摘要。
- [x] 摘要失败时保留旧摘要和原始历史，不能破坏可恢复性。
- [ ] 支持手动重建摘要，便于格式升级和故障恢复。

验收：长会话压缩前后，关键事实和未完成任务可被模型正确回答；原始 JSONL 仍可审计。

### M3：工具结果与 Artifact 管理

- [ ] 为大型工具结果定义 `Artifact` 元数据：ID、类型、路径、大小、摘要、来源 Run。
- [ ] 对超出阈值的工具结果只把摘要和引用放入 WorkingContext。
- [ ] 支持按 artifact ID 或路径重新读取原始内容。
- [ ] 对日志、搜索结果、文件内容提供截断、分页或局部读取能力。
- [ ] Conversation 不保存重复的大型工具正文；Trace 保留必要的原始结果或安全引用。
- [ ] 明确 artifact 的生命周期、清理策略和权限边界。

验收：大工具结果不会持续挤占后续轮次上下文；Agent 仍能通过引用读取所需片段。

### M4：结构化长期 Memory

- [ ] 定义统一 `Memory` 模型：`id`、`session/user scope`、`kind`、`content`、`source`、`confidence`、`importance`、`created_at`、`updated_at`、`expires_at`。
- [ ] 首批支持三类记忆：`fact`（稳定事实）、`episode`（过去事件）、`preference`（用户偏好）。
- [ ] 新增 `MemoryStore` 接口，支持 upsert、search、delete、list 和按 scope 隔离。
- [ ] 第一版优先使用 SQLite + FTS/关键词检索，不先引入独立向量数据库。
- [ ] 检索结果必须带来源、时间和置信度，便于模型判断是否采用。
- [ ] 支持用户显式写入、纠正和删除记忆。
- [ ] 对敏感信息设置默认不写入或需要确认的策略。

验收：新会话能检索到明确授权的稳定事实；不同用户和 session 之间没有记忆串线。

### M5：记忆提取与写入治理

- [ ] 在 Run 成功后异步提取 memory candidates，不阻塞用户看到最终答案。
- [ ] 只写入稳定、可复用且有足够置信度的信息。
- [ ] 为候选记忆保留来源 Run、原文证据和提取原因。
- [ ] 对冲突事实执行版本化或标记待确认，不能静默覆盖。
- [ ] 增加 recency、importance、confidence 的排序和淘汰策略。
- [ ] 支持 TTL、定期清理和按用户请求的完整删除。
- [ ] 增加 prompt injection 防护：工具结果或外部文档不能直接改变记忆写入策略。

验收：普通闲聊不会制造大量低质量记忆；用户说“忘记这件事”后后续检索不再返回该记忆。

### M6：可观测性、恢复与评测

- [ ] Trace 增加 context build、compaction、memory retrieval、memory write 事件。
- [ ] 记录上下文 token、压缩前后消息数、检索命中数、写入/拒绝数量和耗时。
- [ ] 进程重启后能恢复 summary、memory index 和 artifact metadata。
- [ ] 处理 JSONL/SQLite 尾部半写入、索引损坏和摘要版本升级。
- [ ] 增加 deterministic provider 场景，断言每轮完整 messages。
- [ ] 增加长会话、跨会话事实、冲突记忆、删除记忆、超大工具结果和并发 session 测试。
- [ ] 建立 context 质量指标：事实召回率、无关内容比例、token 成本、压缩后任务成功率。

## 建议接口

```go
type ContextManager interface {
    Build(ctx context.Context, req ContextRequest) (ContextResult, error)
    Compact(ctx context.Context, sessionID string) error
}

type MemoryStore interface {
    Search(ctx context.Context, query MemoryQuery) ([]Memory, error)
    Upsert(ctx context.Context, memories ...Memory) error
    Delete(ctx context.Context, ids ...string) error
}
```

`Loop` 只负责生命周期和路由；`ContextManager` 负责本轮上下文；`SessionStore` 负责原始 Conversation；`MemoryStore` 负责长期记忆；`Trace` 负责完整运行事实。

## 暂不实现

- [ ] 不把全部 Trace 自动回放给模型。
- [ ] 不默认保存 reasoning、密码、token、密钥和完整外部网页内容。
- [ ] 不在第一阶段引入多 Agent 共享记忆。
- [ ] 不在没有评测数据前引入复杂的自动记忆重写和多级向量数据库。
- [ ] 不把 system prompt 和用户长期记忆混成同一个不可审计字符串。

## 完成定义

当以下条件全部满足时，认为 Context/Memory 第一阶段完成：

- [ ] 长会话在 context limit 内稳定运行，并且压缩过程可追踪、可恢复。
- [ ] 工具大结果不会造成历史持续膨胀。
- [ ] 跨会话记忆按 scope 隔离，可检索、可删除、可纠正。
- [ ] Conversation、Trace、WorkingContext、Memory 的边界在代码、文档和测试中一致。
- [ ] 关键路径有 deterministic 测试，且 token 成本和召回质量有基础指标。
