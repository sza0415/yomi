# Yomi 分层上下文压缩设计

状态：设计稿，尚未全部实现。

本文把上下文工程章节中的“生产级分层压缩机制”落到 Yomi 当前的
`Conversation`、`Trace`、`WorkingContext` 和工具执行模型上。目标不是把所有历史
都压成一段摘要，而是让不同生命周期、不同价值的信息使用不同的处理方式。

## 1. 当前基础

Yomi 已经具备第一版上下文预算和 rolling summary：

- `internal/agent/ContextManager` 在构造模型请求前加载历史、估算 token，并在超预算时调用当前 Provider 生成摘要。
- `SessionStore` 保留原始 Conversation，并将摘要单独写入 `<session>.summary.json`。
- 摘要覆盖范围由 `covered_count` 标记，重复构造上下文时不会重复压缩已经覆盖的历史。
- `Loop` 将 `ContextManager` 生成的压缩事件写入 Trace，便于观察压缩前后 token 和覆盖范围。
- `Runner` 的工具调用和工具结果属于本轮 WorkingContext，并完整记录到 Trace；Conversation 只保存用户消息和最终 assistant 正文。

这使 Yomi 已经有了“摘要 + 最近历史”的基础能力，但当前仍存在几个边界：

1. 已实现第一版 Artifact 外置和 `artifact_read`，但还没有完整的 Artifact 生命周期治理、过期清理和复用策略。
2. 没有对低价值工具输出做显式删除，也没有记录删除原因。
3. 压缩只有一种 rolling summary，没有按上下文压力选择不同层级。
4. 已实现归档式的压缩检查点，但还没有按当前任务从多个归档中选择性检索。
5. 没有 API 层上下文编辑适配和缓存重建观测。
6. 全量压缩失败没有独立的失败计数和熔断状态。

## 2. 目标与边界

### 目标

- 在不超过模型上下文预算的前提下，优先保留当前任务所需的信息。
- 大工具结果不持续占用主上下文，同时保留可重新读取的来源。
- 保持固定 system prompt 和未发生编辑的前缀稳定，减少 KV Cache 失效。
- 原始 Conversation 和 Trace 可审计、可恢复，WorkingContext 可解释。
- 压缩失败时不破坏旧摘要、原始历史或当前用户请求。
- 为未来的子 Agent / 隔离式探索预留边界。

### 非目标

- 本阶段不删除原始 Conversation 或 Trace。
- 不把 Trace 全量回放给模型。
- 不把所有对话自动提取成跨会话长期 Memory。
- 不在没有 Provider 能力和评测数据的情况下引入复杂向量检索。
- 不承诺摘要是无损的；无损要求通过原文归档、Artifact 引用和 Trace 查询实现。

## 3. 核心概念

```text
Conversation   面向后续会话的主线历史，保留原始消息
Trace          面向审计、排障和 UI 的完整 Run 事实，不回放给模型
WorkingContext 本次实际发送给模型的消息集合
Artifact       大型工具结果或文件内容的外部引用、摘要和元数据
Archive        按轮次保存的结构化摘要，保留任务演进过程
Compression    针对 WorkingContext 的有损减法，不覆盖原始数据
```

约束关系：

```text
原始输入 / 工具结果
        ├── Conversation：主线消息
        ├── Trace：完整事实
        └── Artifact：大型内容的可寻址副本

Conversation + Archive + Artifact preview + 当前请求
        └── ContextManager
                └── WorkingContext
                        └── Runner / Provider
```

## 4. 分层策略

分层策略按“成本从低到高、影响从局部到全局”组织。每一层都应先判断是否能解决
当前预算问题，只有不够时才进入下一层。

| 层级 | 策略 | 处理对象 | 主要触发 | 是否调用 LLM | 主要代价 |
|---|---|---|---|---|---|
| L1 | 工具结果预算控制 | 当前 Run 的大工具输出 | 单条结果超过预算 | 可选，仅生成预览 | 原始结果需要外置存储 |
| L2 | 噪声直接删除 | 低价值或已消费的上下文片段 | 相关性/使用情况明确 | 否 | 可能丢失未显式标记的信息 |
| L3 | API 层微压缩 | 服务端上下文中的旧工具结果 | 使用率接近上限 | 否，由服务端执行 | 编辑点之后的 KV Cache 重建 |
| L4 | 归档式摘要 | 已完成的历史轮次 | 每轮或阶段完成 | 是 | 额外调用和摘要存储 |
| L5 | 全量压缩 | 仍然过大的完整历史 | L1-L4 后仍超限 | 是 | 有损、昂贵、可能失败 |

### L1：工具结果预算控制

工具执行完成后，先根据结果大小和工具类型决定是否生成 Artifact：

- 小结果直接保留为 `role=tool` 消息。
- 大结果写入 Artifact 文件，模型消息只保留结构化预览和引用。
- 预览至少包含 `artifact_id`、工具名、来源 Run、大小、前后片段或摘要、读取方式。
- 一旦某条结果被替换成 Artifact 引用，本次 Run 内不再恢复为完整正文，保证上下文形状稳定。
- Trace 记录原始结果或其安全引用；Conversation 不重复保存大正文。

示例：

```text
[artifact: art_123]
tool: web_fetch
size: 184 KB
preview: 页面包含 3 个与当前查询相关的候选事实
source: https://example.com/page
read: use artifact_read with artifact_id=art_123 and optional range/query
```

工具结果预算不是简单按字符截断。对于代码、日志和搜索结果，应优先支持局部读取、
分页或按行范围读取，使模型可以在发现预览不足时主动取回原文片段。

### L2：噪声直接删除

删除只作用于 WorkingContext，不删除磁盘上的 Conversation、Trace 或 Artifact。
适合删除的内容包括：

- 工具结果中明显的导航栏、页脚、重复广告和格式噪声；
- 已被后续事实取代的临时搜索页面正文；
- 对当前任务无关、且没有被引用的中间探索结果；
- 重复的工具结果或重复的状态注入。

删除必须产生可观测记录：`source_message_id`、`reason`、`estimated_tokens_saved` 和
`reversible_source`。如果无法判断某片段是噪声，不能在 L2 静默删除，应升级到 L4/L5
的语义压缩。

### L3：API 层微压缩

当模型服务支持上下文编辑时，ContextManager 可以提交“从上下文前缀移除指定工具
结果”的编辑操作。Yomi 的本地原始消息不变，服务端请求上下文发生变化。

使用规则：

- 只在上下文使用率接近上限时触发，默认阈值建议为 80% 至 85%。
- 尽量一次批量移除多个已标记、低价值的工具结果。
- 不频繁执行，因为编辑点之后的缓存需要重建。
- 必须记录编辑前后 token、删除的消息标识、服务端能力和缓存 usage。
- Provider 不支持上下文编辑时直接跳过，不影响其他层级。

这层不是本地历史的替代品。它只是减少当前 API 请求的输入；原始 Conversation、Trace
和 Artifact 仍按本地规则保存。

### L4：归档式摘要

归档摘要用于保持长期任务的逻辑脉络，不把所有历史不断 squash 成一条 summary。每个
已完成阶段生成一条独立的 ArchiveRecord：

```json
{
  "id": "arc_20260824_001",
  "session_id": "cli:local",
  "run_id": "run-123",
  "covered_from": 0,
  "covered_to": 12,
  "content": {
    "facts": ["..."],
    "constraints": ["..."],
    "decisions": ["..."],
    "unfinished": ["..."],
    "failures": ["..."],
    "sources": ["artifact:art_123"]
  },
  "created_at": "2026-08-24T00:00:00Z",
  "schema_version": 1
}
```

摘要应优先记录：

- 已确认的事实和事实来源；
- 用户约束、验收标准和权限边界；
- 已做出的设计/实现决策及其原因；
- 已尝试但失败的路径，避免 Agent 重复执行；
- 当前未完成事项、风险和下一步；
- 关键文件路径、函数名、Artifact ID 或外部 URL。

WorkingContext 默认只放最近若干条 ArchiveRecord 和与当前问题相关的记录。历史归档
仍保留在磁盘上，可按任务需要重新检索。

当前实现先完成归档的持久化闭环：每次 rolling summary 成功后，在
`archives/<session>.jsonl` 追加一条独立记录，包含 `run_id`、覆盖范围、消息数量、摘要
和可识别的 `FACTS/CONSTRAINTS/DECISIONS/UNFINISHED/FAILURES/SOURCES` 分段。它不会替换
原始 Conversation，也不会让归档自动全部回放给模型；选择性检索属于后续阶段。

### L5：全量压缩

全量压缩是预算恢复的最后手段。建议的决策顺序为：

```text
加载原始 Conversation
  -> L1：外置大工具结果
  -> L2：删除已确认噪声
  -> L3：API 微压缩（若可用且接近上限）
  -> L4：压缩已完成轮次为 ArchiveRecord
  -> 仍超限：先压缩会话记忆/归档
  -> 仍超限：全量生成新的结构化 summary
  -> 仍超限：保留当前请求 + 最小必要最近历史，否则返回预算错误
```

全量压缩要满足：

- 当前用户消息永远不被丢弃；
- 新摘要成功持久化后才能替换 WorkingContext；
- 摘要失败时继续使用旧摘要和原始历史，不能覆盖旧状态；
- 摘要生成采用版本号和幂等键，避免同一覆盖范围重复膨胀；
- 连续失败达到阈值后熔断，短时间内不再自动调用压缩模型；
- 熔断状态写入 Session 元数据，并在 Trace 中记录恢复原因。

## 5. ContextManager 的职责

`ContextManager` 负责“本次请求应该给模型看什么”，不负责删除原始数据。建议逐步
将当前 `Build` 扩展为策略管线：

### 5.1 统一上下文预算控制

上下文窗口可以理解为一个固定容量的容器。`ContextManager` 负责跨 Run 加载和整理
历史，但 `Runner` 在单次 Run 内还会不断追加 assistant tool call 和 tool result。因
此，预算检查不能只发生在新一轮用户请求开始时，而应当在每次模型调用和每次工具结果
加入前使用同一套规则。

统一预算组件至少需要统计：

```text
system prompt
+ Conversation / Archive 历史
+ 当前用户消息
+ assistant tool call
+ 已加入的 tool result 或 Artifact preview
+ 工具定义
+ 模型输出预留
```

它关注的是“加入下一条消息以后会怎样”，而不是只看当前已经使用了多少：

```text
candidate_tokens = current_messages
                 + next_tool_result
                 + tool_definitions
                 + output_reserve
```

其中 token 估算可以先使用当前的近似算法，Provider 如果提供真实 tokenizer 或 usage，
再逐步校准。预算组件不需要一开始就做到精确，但必须在所有上下文入口使用同一种估算
口径。

#### 预算水位和预留

不能把整个上下文窗口都用于输入。模型还需要空间生成最终回答，工具定义也会占用输入
预算。因此至少需要两个独立的预留项：

- `output_reserve_tokens`：为模型最终输出和推理预留的空间；
- `tool_definition_tokens`：当前启用的工具名称、描述和参数 Schema 所占的空间。

当上下文使用率达到窗口的 75% 到 80% 时进入预警区，而不是等到 100% 才处理。预警区
允许系统在下一个大工具结果到来前做出决定：外置为 Artifact、生成 preview、删除明确
噪声、归档历史或启动更强的压缩。

#### 工具结果进入前的决策

```text
工具执行完成
  -> 估算加入完整结果后的 candidate_tokens
  -> 未进入预警区：完整加入 WorkingContext
  -> 进入预警区或超过预算：按分层策略处理
       -> L1 Artifact preview
       -> L2 明确噪声删除
       -> L3 API 层微压缩
       -> L4 归档摘要
       -> L5 全量压缩
```

预算控制只改变本次 WorkingContext，不删除 Conversation、Trace 或 Artifact 原文。每个
决策都应记录层级、原因、压缩前后 token 和是否可恢复。

#### 必须保护的消息

预算不足时不能静默删除当前任务的关键消息。最低保护集合是：

- 当前用户消息；
- 当前 assistant 的 tool call；
- 与该 tool call 配对的 tool result 或 Artifact 引用；
- 最近一轮工具交互；
- 当前任务的约束、未完成事项和已确认决策。

特别是 `assistant(tool_call) -> tool(result)` 不能只删除其中一条，否则可能生成不符合
Provider 协议的消息序列。预算确实无法满足时，应保留当前请求和最小保护集合，并返回
可观测的 `context_budget_exceeded`，而不是静默截断用户输入。

#### 与现有实现的对应关系

当前已经在 `Runner` 和 `ContextManager` 之间共享 `ContextBudget`：它统一计算消息、
工具定义和模型输出预留，并提供 80% 预警和超限判断；超过限制的工具结果会进入
Artifact。后续仍需把归档摘要、噪声删除和 Provider API 编辑纳入同一条策略管线，避免
`ContextManager` 和 `Runner` 各自维护不同的分层规则。

```go
type ContextRequest struct {
    SessionID    string
    SystemPrompt string
    User         providers.Message
    Model        string
    MaxTokens    int
    Tools        []providers.ToolDefinition
}

type ContextResult struct {
    Messages        []providers.Message
    EstimatedTokens int
    HistoryCount    int
    Decisions       []ContextDecision
    Compaction      *CompactionResult
}

type ContextDecision struct {
    Layer           string // artifact, delete, api_edit, archive, full
    SourceID        string
    Action          string
    Reason          string
    TokensBefore    int
    TokensAfter     int
    Reversible      bool
}

type CompactionPolicy interface {
    Apply(ctx context.Context, req ContextRequest, state ContextState) (ContextState, error)
}
```

第一阶段不必一次引入所有接口。现有 `ContextManager.Build` 可以继续作为外部入口，
内部先抽出以下纯职责：

1. `estimateBudget`：计算 system、历史、工具定义和当前请求的预算。
2. `selectCandidates`：按照消息类型、大小、来源和使用标记选择候选。
3. `applyLocalDecisions`：执行 Artifact 引用和噪声移除，只改变 WorkingContext。
4. `buildArchive`：对已完成范围生成逐轮归档摘要。
5. `compactFallback`：执行现有 rolling summary，并增加失败熔断。

## 6. 持久化设计

### Conversation

继续保留现有 JSONL 主线，不做原地重写。原始历史是恢复和审计的底线。

### Session 元数据

在现有 summary 元数据基础上扩展字段，建议仍使用原子替换写入：

```json
{
  "summary_version": 2,
  "covered_count": 24,
  "summary": "...",
  "compaction_failures": 1,
  "compaction_circuit_open_until": 0,
  "last_compaction_at": 1787529600
}
```

`covered_count` 只能向前推进；新摘要写入失败时不得更新覆盖范围。

### Artifact

建议目录结构：

```text
sessionlogs/
├── conversations/
├── summaries/
├── archives/
├── artifacts/
└── traces/
```

第一版 Artifact 可以使用每个 Session 独立的 JSON 元数据和内容文件，避免立即引入
数据库。元数据至少包含：

```text
id, session_id, run_id, tool_name, path, size_bytes,
content_type, sha256, preview, source, created_at, expires_at
```

Artifact ID 必须按 Session 隔离校验，不能仅凭用户提供的路径读取任意文件。文件权限
沿用 SessionStore 的 `0700/0600` 边界。

## 7. KV Cache 不变量

实现时必须保持以下不变量：

1. 固定 system prompt 位于上下文最前面。
2. 不把每轮变化的时间、计数器和工具列表顺序插入稳定前缀。
3. L1 的替换决策在一次 Run 内冻结，不在每次模型调用时重新判断。
4. L2-L5 的修改只影响被修改点及其之后的上下文；不要为了删除尾部噪声重写前缀。
5. API 微压缩必须批量触发，不按每一轮工具调用单独执行。
6. 只有摘要成功落盘后，下一轮才使用新摘要。
7. Trace 记录压缩决策，但 Trace 本身不进入模型上下文。

压缩和缓存并不矛盾：缓存服务重复的稳定前缀，压缩控制长尾上下文的上限。正确的
实现是让稳定前缀尽量不变，并在接近窗口上限时集中处理后缀历史。

## 8. 失败恢复与安全

- Artifact 写入失败：保留完整工具结果在当前 WorkingContext；不要生成失效引用。
- 摘要模型超时或返回空内容：保留旧摘要，记录失败事件，按熔断策略处理。
- 摘要持久化失败：本次请求可以使用内存结果，但不能推进覆盖游标；更稳妥的默认行为是返回可观测错误并保留原始历史。
- Artifact 元数据损坏：跳过该引用并提示模型原文不可用，不读取未经校验的路径。
- 上下文仍超限：保留 system、当前 user、最小必要状态和最近消息，返回 `context_budget_exceeded`，不能静默截断当前输入。
- 外部网页、工具输出和 Artifact 内容都是不可信数据，不能让其中的指令修改压缩策略或记忆写入策略。
- Trace 继续按现有敏感数据边界处理；压缩摘要可能包含用户输入、文件路径和外部来源，不能默认视为脱敏数据。

## 9. 可观测性

现有 `context.compacted` 事件继续保留，并扩展为可区分各层：

```text
context.strategy.applied
  layer: artifact | delete | api_edit | archive | full
  source_id: ...
  action: replace | remove | summarize | circuit_open
  tokens_before: ...
  tokens_after: ...
  reversible: true | false
```

当前 Trace 只记录一条 `context.strategy.applied` 事件，同时承载上下文处理的决策和
实际应用结果。它记录 `context_id`、阶段、触发原因、尝试过的层、最终层、动作、
token 变化、可恢复性以及相关 Artifact。完整原文仍由 `tool.execution.finished`、
Artifact 和实际的 `model.request.started.messages` 提供，避免重复写入决策事件。

每次模型请求至少记录：

- WorkingContext 消息数和估算 token；
- system、archive、conversation、artifact preview、当前请求各自的 token 占比；
- 触发的层级和跳过的层级；
- 压缩耗时、调用次数、失败次数和熔断状态；
- Provider 返回的 input/cached/total usage（如果提供）。

这些数据用于回答三个问题：压缩是否真的减少 token、是否保留了任务关键事实、是否
因为频繁编辑导致缓存命中下降。

## 10. 分阶段实现

### P0：文档与现状校准

- [x] 保留当前 `ContextManager` 行为，补齐事件字段和测试说明。
- [x] 抽出统一预算口径，明确工具定义和模型输出预留，以及 75% 到 80% 的预警区。
- 明确 Conversation、Trace、WorkingContext、Artifact 的边界。
- 增加 deterministic Provider 场景，锁定压缩前后消息顺序。

### P1：L1 工具结果 Artifact

- [x] 新增 Artifact 元数据和受 Session 隔离的存储接口。
- [x] 在统一预算组件判断后，于工具结果进入下一次模型请求前按大小阈值生成 preview/reference。
- [x] Trace 记录原始结果大小、Artifact ID 和替换决策。
- [x] 增加 `artifact_read` 或等价的局部读取能力。

### P2：L2 + L4 本地策略

- 为工具结果增加 `relevance`、`consumed`、`source` 等内部元数据。
- 先实现明确规则的噪声删除，不做自动语义判断。
- [x] 增加 ArchiveRecord，按阶段保留结构化摘要，不替换原始 Conversation。

### P3：L5 全量压缩治理

- 将现有 rolling summary 改为结构化摘要提示和版本化元数据。
- 增加摘要失败计数、退避和熔断器。
- 增加手动重建摘要入口，支持格式升级和故障恢复。

### P4：L3 Provider 能力

- 在 Provider 能力接口中增加可选的上下文编辑能力。
- 仅对支持该能力的 Provider 启用 API 微压缩。
- 通过 Trace 和 usage 验证编辑带来的缓存重建成本。

### P5：评测与子 Agent 隔离

- 对“完整保留、单条摘要、rolling summary、分层压缩”做相同任务对比。
- 评估事实召回率、重复工具调用率、输入 token、压缩成本、缓存 token 和成功率。
- 对大范围搜索和代码探索优先增加子 Agent 隔离，让中间结果不进入主 Agent 上下文。

## 11. 验收标准

- 100KB 以上工具结果不会在后续请求中重复进入主上下文，但仍能通过 Artifact ID 读取。
- 已确认的低价值工具结果能从 WorkingContext 删除，且原始 Trace/Artifact 不丢失。
- 压缩前后能保留用户约束、关键决策、失败路径、未完成任务和来源引用。
- 同一压缩任务重复执行具有幂等性，不会让摘要持续膨胀。
- 摘要连续失败达到阈值后停止自动重试，恢复条件清晰可观测。
- 当前用户消息、system prompt 和必要的最近状态不会被静默丢弃。
- 不同 Session 的 Conversation、Archive 和 Artifact 不会互相读取。
- 评测能证明分层策略在 token 成本和任务成功率之间优于单一 rolling summary。

## 12. 设计结论

Yomi 的落地顺序应当是：先把大结果变成可寻址 Artifact，再删除明确噪声；接近窗口
上限时才做 API 层批量编辑；长期任务用逐轮归档保持脉络；只有这些手段仍不足时，才
调用 LLM 做全量压缩，并对失败进行熔断。

这样做的关键收益是：原始事实仍可恢复，主上下文保持高信息密度，缓存失效集中在可
接受的时机，压缩策略也能通过 Trace 被解释和评估。对于天然会产生大量中间信息的任
务，进一步使用子 Agent 隔离通常比事后压缩更优。
