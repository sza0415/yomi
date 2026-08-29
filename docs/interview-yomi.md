# yomi 面试复习稿

## 一、核心架构

核心链路：

```text
Channel -> MessageBus -> AgentLoop -> Runner -> Provider
                              |
                              +-> Tool / Context / Store / Trace / Snapshot
```

### MessageBus

- 只负责统一入站、出站消息传输，以及 Go channel 提供的并发安全和背压。
- 不理解 Session、模型协议或具体 Channel。
- `InboundMessage` 至少包含 `SessionID`、`ChannelID`、用户文本和元数据。
- `OutboundMessage` 携带 `SessionID`、`RunID`、`Sequence`、内容类型和流式标记。

### AgentLoop

负责外层运行编排：

- 消费 MessageBus 入站消息；
- 按 Session 调度 Run；
- 加载 Conversation 和 system prompt；
- 执行上下文预算、压缩和 Artifact/Archive 协调；
- 创建 Run Context，处理超时和显式取消；
- 将 Runner 事件转换成 OutboundMessage；
- 持久化 Conversation、Trace 和 Run Snapshot；
- 处理 `ask_user_question` 的提问和答案路由。

### Runner

负责一次 Run 内部的模型执行循环：

- 调用 Provider；
- 处理流式和非流式响应；
- 识别 Tool Call；
- 执行工具并把结果追加到模型上下文；
- 进行有限重试、错误分类、状态迁移和预算检查；
- 通过 `StreamSink` 报告 reasoning、正文、模型和工具事件；
- 直到得到最终 assistant 回复，或发生取消、超时、预算耗尽、不可重试错误。

一句话边界：

> AgentLoop 决定请求何时运行、使用哪个 Session 上下文、结果发到哪里以及如何持久化；Runner 决定这一轮如何与模型和工具交互直到得到最终答案。

## 二、一次请求链路

1. Channel 将 CLI/Web 原生请求翻译为 `InboundMessage`。
2. `PublishInbound` 写入 MessageBus。
3. AgentLoop 消费消息；如果 Session 正在等待用户回答，则直接交给等待中的工具，否则创建 `Run` 并入队。
4. `drainSession` 取队首 Run，创建带取消/超时能力的 Context。
5. Loop 加载历史、拼接 system + history + 当前 user，并执行预算控制。
6. Loop 调用 `Runner.RunCollect`。
7. Runner 调用 Provider；如有 Tool Call，则执行工具、追加结果并继续请求模型。
8. Runner 通过 `StreamSink` 向 Loop 报告模型、工具、reasoning 和正文事件。
9. Loop 将事件写入 Trace，并将实时内容写回 Outbound Bus。
10. 成功时 Conversation 追加本轮 user 和最终 assistant；Run Snapshot 转为终态；发送 `Done`。
11. Channel 从 Outbound Bus 读取并渲染到 CLI 或 Web SSE。

## 三、Session 调度与并发

调度器使用：

```go
mu       sync.Mutex
queues   map[string][]queuedRun
draining map[string]bool
```

### FIFO 与单消费者

入队时在同一把锁内完成追加和检查：

```go
l.queues[sessionID] = append(l.queues[sessionID], item)
start := !l.draining[sessionID]
if start { l.draining[sessionID] = true }
```

因此同一个 Session 最多有一个 `drainSession` goroutine。它始终从队首取 Run，并在锁外同步执行 `handleRun`：

```text
Session A: A1 -> A2 -> A3
Session B: B1 -> B2
```

这里的 A1、A2、A3 是同一 Session 下三条独立用户请求对应的三个 Run，不是一个 Run 的三个模型步骤。

### 跨 Session 并行

每个活跃 Session 使用自己的 drain goroutine，因此不同 Session 可以并行执行。当前不是固定 worker pool，也不是每个 Run 一个 goroutine。

### 回收与竞态

队列为空时，在锁内删除 `queues[sessionID]` 和 `draining[sessionID]`，然后退出 goroutine。新消息若在回收前到达，会被现有消费者处理；若回收先完成，则新消息重新创建消费者。由于入队和回收使用同一把锁，不会丢消息或创建两个消费者。

### 当前背压边界

当前每个 Session 的 slice 队列无界，MessageBus 入口有缓冲但不能限制 Loop 内存增长。后续可增加单 Session 队列上限、全局并发 semaphore、排队 Run 取消、队列指标和公平调度。

## 四、Run 与 Context 取消

### Run 是什么

一条用户请求通常对应一个 Run。Run 有独立的 `RunID`、状态、预算、usage、Trace 和 Snapshot。

Run 内部可能包含多次 Model Call 和 Tool Call：

```text
Run A1
  -> Model Call 1
  -> Tool Call / Result
  -> Model Call 2
  -> Final Answer
```

### Context 创建时机

- 入队时先创建 `Run` 对象，状态为 `queued`，但还没有独立 cancel handle。
- `handleRun` 真正开始执行时才创建 `context.WithCancel` 或 `context.WithTimeout`。
- `RunTimeout` 从 Run 开始执行时计时，不包含 Session 队列等待时间。
- `running[sessionID]` 保存当前 Run 的 cancel 函数。

### 显式取消链路

```text
POST /api/cancel
  -> WebChannel.handleCancel
  -> OnCancel(sessionID)
  -> Loop.CancelSession
  -> runHandle.cancel()
  -> runCtx.Done()
  -> Runner / Provider / Tool 感知取消
```

Context 会传给：

- `Provider.Chat` / `ChatStream`；
- `Tool.Execute`；
- HTTP 工具的 `NewRequestWithContext`；
- Docker 沙盒的 `exec.CommandContext`；
- Retry 等待和 `ask_user_question` 等待。

取消的是当前 Session 正在执行的 Run，不会自动删除尚未开始的排队 Run。SSE 断连也不会触发取消。

## 五、SSE 断连与重连

### 两条不同路径

关闭标签页或网络断开：

```text
r.Context().Done()
  -> SSE handler 返回
  -> removeSubscriber
  -> Run 继续执行
```

用户点击取消按钮：

```text
POST /api/cancel
  -> CancelSession
  -> 当前 Run Context 被取消
```

系统根据是否收到显式取消 API 判断意图，不根据 SSE 连接状态推断。

### 当前重连能力

浏览器用 `localStorage` 恢复相同 `SessionID`，重连 `/api/stream`。当前 SSE 不做历史事件 replay：断线期间的实时 token delta、reasoning 和工具展示事件不会自动补发。

但 Run 状态和执行事实会持久化，可通过：

```text
GET /api/runs?session=<sessionID>
GET /api/traces/run?run_id=<runID>
```

查看 `running`、`completed`、`failed` 等状态，以及已落盘的 Model/Tool/Context 轨迹。当前可以恢复状态和持久化轨迹，但不能完整恢复断线期间的实时流。

后续可使用 `RunID + Sequence`、`Last-Event-ID` 或 `after_sequence` 实现 SSE 事件补发。

## 六、Trace

Trace 是按 Run 记录的执行事实流，使用 JSONL 逐事件写入，不等 Run 结束后批量写入。Conversation 和 Trace 分工不同：

```text
Conversation -> 下一轮模型上下文，只保存成功 Run 的 user + final assistant
Trace        -> 完整运行事实，不参与下一轮上下文
```

### Run 状态机

```text
queued
  -> running
  -> waiting_user
  -> completed / failed / cancelled / timed_out / budget_exceeded
```

### Model 状态机

```text
idle -> requesting -> streaming -> finished
                       \-> errored
```

Model 没有独立的 `timeout` 枚举；超时通常在 Model 层表现为 `errored`，Run 层根据 `context.DeadlineExceeded` 标记为 `timed_out`。

### Tool 状态机

```text
pending -> running -> succeeded / failed / timed_out
```

重试由 RetryPolicy 管理，不是独立的 `retry_failed` 状态；多次尝试后最终进入 `failed` 时，Trace 会记录错误和尝试信息。

### 其他 Trace 事件

除了三层状态，Trace 还记录：

- 用户输入、system prompt、assistant 完整消息；
- 实际模型请求、工具定义、Provider、Model、usage、耗时和首 token 时间；
- Tool 参数、结果、结果大小和错误；
- `context.injected`、`context.compacted`、`context.archived`、`context.strategy.applied`；
- Artifact 创建、用户提问和回答。

Trace 的写入时机是事件发生时立即 `Record`；默认 JSONL sink 加锁、追加一行并 Flush。它不是每条事件都 `fsync`，所以是逐事件持久化，但不是最强崩溃恢复保证。

## 七、Run Snapshot

Snapshot 解决“如何快速知道任务当前状态，以及进程重启后如何识别遗留 Run”的问题。

它保存：

```text
RunID / SessionID / Status / Error / Budget / Usage
QueuedAt / StartedAt / FinishedAt / UpdatedAt
```

用途：

1. Web 快速查询 Run 状态，不必扫描完整 Trace；
2. 每个 Run 一个原子替换的 JSON 文件，避免半截快照；
3. 启动时 `MarkInterrupted`，把之前处于非终态的 Run 标记为“interrupted by process restart”；
4. 不重放工具，避免重启后重复执行副作用操作。

Snapshot 是状态摘要，不替代 Trace，也不保存完整 Conversation。

## 八、Context 分层压缩

统一预算同时计算：

```text
system prompt
+ Conversation / rolling summary
+ 当前 user
+ tool definitions
+ output reserve
```

### 当前已实现顺序

#### 当前 Run 内的 Tool Result

```text
完整结果
  -> 工具专属 preview
  -> Artifact 外置 + 引用
  -> Artifact 不可用时 bounded preview / truncate
```

这是局部、低损失、可逆的处理，原始结果仍在 Artifact/Trace 中。

### Artifact 外置与工具专属 Preview 的区别

两者解决的是不同问题：

> Artifact 外置解决“完整原文放在哪里”；工具专属 Preview 解决“模型在上下文里看到什么”。

**Artifact 外置**是存储和恢复策略。当工具结果过大或加入后会导致上下文超限时，系统将完整结果写入 `ArtifactStore`，生成 `artifact_id`，再在模型上下文中放入引用、结果大小、少量预览以及通过 `artifact_read` 恢复原文的提示。原始结果仍然保留，后续可以按范围读取。

**工具专属 Preview**是上下文表示策略。系统根据工具语义选择最有价值的信息并进行压缩：

- `web_search` 优先保留搜索摘要、结果标题和 URL；
- `web_fetch` 优先保留页面标题和来源 URL，再压缩正文；
- 其他工具使用通用的头尾截取。

典型流程是：

```text
完整 Tool Result
  -> 预算检查
  -> 生成工具专属 preview
  -> 尝试 Artifact 外置
       ├── 成功：preview + artifact_id 进入上下文
       └── 失败：仅使用 bounded preview，并记录降级原因
```

因此两者不是互斥方案，而是两个维度：Artifact 负责保留完整原文并提供恢复入口，Preview 负责控制模型请求中的 token 和信息重点。Artifact 不可用时，Preview 仍可单独作为兜底，但模型只能看到压缩后的内容。

#### 跨 Run 的 Conversation 历史

```text
较早历史
  -> Provider 生成 rolling summary
  -> 保存 summary + covered cursor
  -> 追加 ArchiveRecord
  -> 保留最近消息
  -> 最近窗口仍超限时删除更早 recent 消息
```

如果仍无法满足预算，当前 Runner 会显式返回 `context_budget_exceeded`，不会静默截断当前 user。

### 为什么叫“分层”

不同数据层采用不同策略：

```text
system / 当前 user / 必要状态 -> 尽量保留
tool definitions              -> 参与预算，保持稳定
大型 Tool Result              -> preview / Artifact
较早 Conversation             -> rolling summary
原始记录                      -> Archive / Trace / Artifact
output reserve                -> 作为硬预算扣除
```

总体原则是：先处理局部、可逆、低损失对象，再处理旧历史，最后才失败；同时尽量保持稳定前缀，利于 Provider 的缓存命中。

### Rolling Summary 与 ArchiveRecord

- **Rolling Summary**：当前最新的、会直接注入下一轮模型上下文的可变摘要，带 `covered_count`。
- **ArchiveRecord**：每次压缩追加的不可变检查点，用于审计、回溯和恢复，默认不全部注入模型。

原始 Conversation 不删除；Summary 是工作台，Archive 是历史快照。

### 当前边界

已实现 Tool Result Artifact/preview、Conversation rolling summary 和 ArchiveRecord；尚未完全实现对已消费低价值工具结果的主动删除、Provider 原生上下文编辑和同一 Run 内的增量 rolling summary。因此工具 preview 后仍可能触发 `budget_exceeded`。

## 九、面试时的总括回答

> yomi 把 Channel、Tool、Skill 和 Provider 放在核心循环外，通过 MessageBus、AgentLoop 和 Runner 形成稳定主链路。MessageBus 负责统一消息传输，AgentLoop 负责 Session 调度、Context、持久化和路由，Runner 负责模型与工具的多轮执行。同一 Session 使用单消费者 FIFO，跨 Session 通过不同 drain goroutine 并行。Run Context 的取消从显式 API 进入，向下传播到 Provider、Tool 和 Retry；SSE 断连只移除订阅者，不影响后台 Run。Trace 逐事件记录 Run/Model/Tool 状态及请求、结果、错误和上下文策略，Snapshot 保存任务级当前状态并处理进程重启。Context 压缩采用分层策略：先对大工具结果做 preview/Artifact，再对旧 Conversation 做 rolling summary，并用 ArchiveRecord 保留压缩检查点，最终无法安全压缩时显式返回预算错误。
