# yomi Agent Harness 建议

本文整理 yomi 后续可以建设的 Agent harness。这里的 harness 不只指测试代码，也包括包在 AgentLoop / Runner 外部、用于控制运行、注入环境、记录事件、制造故障和评估结果的基础设施。

## 目标

yomi 的 harness 需要保证：

- Agent 行为可以重复运行，不依赖真实模型和网络的偶然结果。
- 工具调用、用户提问、流式输出和取消过程都有清晰状态。
- Provider、工具、持久化、取消和客户端断连等故障可以主动注入。
- 一次运行可以同时检查用户输出、工具轨迹、Trace、Conversation 和资源消耗。

| 方向 | 解决的问题 | 代表能力 |
| --- | --- | --- |
| 运行时 Harness | Agent 执行时如何感知状态、交互和停止 | 状态栏、用户提问、显式取消、流式推送 |
| 测试 / 评测 Harness | 如何重复验证 Agent 行为是否正确 | Scripted Provider、Scenario、事件断言、真实任务集 |
| 安全与可靠性 Harness | 出错、超限、崩溃和越权时如何保持可控 | 故障注入、恢复测试、权限检查、预算和并发 |

## 一、运行时 Harness

运行时 Harness 帮助 Agent 在执行过程中感知状态、和用户交互、处理取消，并把运行中的信息传递给 channel 和 UI。

### 当前已有基础

yomi 已经具备几类运行时 harness 的雏形：

- Agent 状态栏：每轮调用模型前重新注入 todo 状态。
- `ask_user_question`：Agent 暂停并等待用户回答后继续。
- 显式取消：Web 端主动取消任务，并将 context 传播到下游；客户端断连不影响 Run。
- 流式直穿与旁路持久化：实时输出给前端，完整过程写入 Trace 或 Conversation。

当前运行时 Harness 已经覆盖状态感知、用户交互、生命周期管理、三层状态、快照和 Web 状态查询。后续主要补充重试策略和真正的断点续跑。

### 运行时里程碑

下面用 `[x]` 表示当前已经具备，用 `[ ]` 表示后续需要建设：

- [x] H1 Agent 状态栏：每轮模型调用前注入最新任务状态
- [x] H2 `ask_user_question`：Agent 暂停、提问并等待用户回答
- [x] H3 显式取消：Web 端主动取消下游 Agent 任务
- [x] H4 流式直穿与旁路持久化：实时输出和完整 Trace 分离
- [x] H5 三层状态机：统一表示 Run、Model、Tool 的状态和合法转换
- [x] H6 Run Snapshot：保存任务状态，并在进程重启后标记未完成 Run
- [x] H7 Web 状态查询：展示三层状态并支持按 session/status 查询 Run
- [x] H8 运行时重试策略：区分可重试的 Provider/工具错误和不可重试错误

### 运行时示例

```text
用户提交任务
  -> Agent 读取当前 todo 状态
  -> Agent 调用工具并持续推送事件
  -> Agent 需要信息时向用户提问
  -> 用户点击取消时停止下游任务
  -> SSE 断连或重连不影响 Run
```

当前 Web 查询接口：

```text
GET /api/runs?session=<session_id>
GET /api/runs?session=<session_id>&status=cancelled
GET /api/traces/run?run_id=<run_id>
POST /api/cancel {"session":"<session_id>"}
```

## 二、测试 / 评测 Harness

测试 / 评测 Harness 用固定场景验证 Agent 的行为是否正确。它应该隔离真实模型的不确定性，同时检查最终答案、工具调用、上下文、Trace 和持久化结果。

### 1. Deterministic Provider Harness

使用假的 Provider 预先定义每一轮模型返回内容，让 Agent 行为完全可重复。这是其他 harness 的基础。

### 示例

```text
用户：帮我读取 config.yaml 并总结

第 1 次模型调用：
  tool_call(read_file, {"path":"config.yaml"})

工具返回：
  port: 8080
  mode: production

第 2 次模型调用：
  final answer("服务运行在 8080 端口，当前是 production 模式")
```

应检查：

- 每次发送给模型的完整消息。
- 工具调用的名称、参数和顺序。
- 工具结果是否正确回灌到下一轮上下文。
- 最终答案和 `RunResult.Messages`。
- `StreamSink` 事件顺序。
- Trace 和 Conversation 的落盘内容。

### 2. Agent Scenario Harness

用完整用户任务验证 Agent，而不是只测试某个函数。

建议优先覆盖：

| 场景 | 主要验证内容 |
| --- | --- |
| 读取文件并总结 | 基本 tool calling |
| 读取文件后修改 | 多轮工具调用和写入 |
| 工具失败后自行修正 | 错误回灌模型 |
| 连续调用多个工具 | 工具顺序和上下文 |
| Agent 主动提问 | pending 状态和继续执行 |
| 用户执行中断开 | cancellation |
| 同一 session 多轮对话 | history 恢复 |
| 两个 session 并发执行 | session 隔离 |
| 达到 tool turn 上限 | 循环终止 |
| 达到 token budget | 预算控制 |
| Provider 返回非法 tool call | 协议错误处理 |

### 示例：工具失败后修正

```text
第 1 次：read_file("missing.txt")

工具结果：Error: file not found

第 2 次：read_file("README.md")

第 3 次：返回最终总结
```

这个场景可以验证工具错误是否被正确回灌，以及 Agent 是否能够调整策略继续完成任务。

### 测试 / 评测里程碑

- [ ] H9 Deterministic Provider：可脚本化控制每轮模型返回
- [ ] H10 Scenario Runner：用完整用户任务驱动 Agent 执行
- [ ] H11 Event Collector：统一收集 Provider、Tool、Bus、Trace 事件
- [ ] H12 Streaming Contract：断言 reasoning、正文、tool call、Done 顺序
- [ ] H13 真实模型回归：维护固定任务集并比较成功率、成本和延迟

## 三、安全与可靠性 Harness

安全与可靠性 Harness 用于模拟异常、限制资源、验证崩溃恢复，并检查数据、工具和权限边界。

### 1. Fault Injection Harness

专门模拟运行过程中的异常和中断：

- Provider 超时或返回服务端错误。
- Provider 流式输出一半后失败。
- 工具执行超时或返回非零退出码。
- SessionStore 或 Trace 写入失败。
- 客户端断连。
- `ask_user_question` 等待期间取消。
- Provider 返回没有 ID 的 tool call。
- Provider 请求未注册的工具。
- Agent 连续产生无限 tool call。

重点检查：

- `pending` 状态最终一定清理。
- `running` registry 最终一定注销。
- 取消后不能继续调用 Provider 或工具。
- 错误运行不能被记录为 `completed`。
- 已产生的 Trace 不应伪造成功结果。
- 不应因为一次工具失败导致整个进程崩溃。

### 3. Streaming Contract Harness

yomi 同时处理 reasoning、正文、tool call、tool result 和 `Done` 事件，需要单独验证事件协议。

```text
reasoning delta: 先读取文件
content delta:   我来检查配置
tool call:       read_file
tool result:     port: 8080
content delta:   配置显示端口为 8080
done
```

应检查：

- 所有 content delta 拼接后等于最终答案。
- reasoning 不会混入用户正文。
- tool call 的 ID、名称和参数不会丢失。
- `Done` 恰好出现一次。
- 中途失败时仍有明确的终态。
- Web/SSE 保留实时 delta；Trace 保留聚合后的完整消息和结构化执行事件，两者通过 Run/step 关联。

### 4. Persistence / Recovery Harness

yomi 同时维护 Conversation 和 Trace，需要把两者边界固化成不变量：

```text
Conversation：下一轮模型需要的主线历史
Trace：排障、审计和 UI 展示需要的完整运行过程
```

建议覆盖：

- 一轮工具调用后 Conversation 是否只保存约定的主线消息。
- Trace 是否保存完整的模型、工具和状态事件。
- 进程在模型调用后或工具执行后崩溃。
- JSONL 尾部只写入半行。
- 重启后 session 是否可以继续。
- session ID 是否能防止路径穿越。
- 不同 session 是否不会串历史。
- Trace sequence 是否单调递增。

### 5. Security / Policy Harness

yomi 的文件、Bash、Python 和网络工具需要独立的安全 harness。

```text
用户：读取 ../../.ssh/id_rsa

期望：
  工具拒绝访问；
  不泄露文件内容；
  Trace 记录 policy denied；
  Agent 不绕过工具边界继续执行。
```

建议检查：

- `../`、绝对路径和符号链接逃逸。
- workspace 外的文件读写。
- shell 参数注入。
- 环境变量泄漏。
- 超大文件和超大 tool result。
- 未注册工具调用。
- Docker 不可用或沙盒超时。
- tool result 中包含恶意 prompt injection 时的边界行为。

真实 Docker 测试应作为 integration test 单独运行；普通单元测试使用 fake sandbox，避免 `go test ./...` 依赖本机 Docker daemon。

### 6. Budget / Cost Harness

使用 `RunBudget` 验证：

- 超过模型调用次数后停止。
- 超过工具调用次数后停止。
- 超过总 token 后进入 `budget_exceeded`。
- 达到预算后不再调用 Provider 或工具。
- Trace 记录实际 usage 和失败原因。
- 已生成的部分结果按约定保留。

### 7. Concurrency Harness

AgentLoop 中的 session queue、running registry 和 pending question 都需要并发测试。

```text
Session A：请求 1 正在执行，请求 2 同时进入
期望：请求 2 排队，不与请求 1 并行执行

Session B：同时执行另一个任务
期望：Session B 不被 Session A 阻塞

Web：同一 session 打开两个 tab，关闭其中一个
期望：不能取消仍在使用的 session
```

建议持续运行：

```bash
go test -race ./...
```

### 8. 真实模型回归 Harness

Deterministic harness 用于验证框架语义，真实模型 harness 用于验证 Agent 是否能完成实际任务。

固定任务可以包括：

```text
1. 找出 Go 项目中所有 TODO。
2. 修改一个指定函数并运行测试。
3. 读取日志后定位错误原因。
4. 修改配置文件，但不能触碰 workspace 外文件。
5. 信息不足时主动向用户提问。
```

每次运行保存任务输入、模型和提示词版本、工具列表、最终答案、工具轨迹、Trace、token、延迟、成本和失败原因。

可使用的指标包括：

- task success rate；
- tool call success rate；
- recovery rate；
- 平均模型调用次数和工具调用次数；
- 首 token 延迟和总耗时；
- token 消耗和成本；
- 未授权文件访问次数；
- 无效工具调用次数；
- 用户介入次数。

### 安全与可靠性里程碑

- [ ] H14 Fault Injection：注入超时、取消、工具失败和写盘失败
- [x] H15 Persistence Recovery：验证 Run Snapshot 和保守的进程重启恢复
- [ ] H16 Security / Policy：验证 workspace、沙盒和工具权限边界
- [ ] H17 Budget / Cost：验证 token、模型调用和工具调用上限
- [ ] H18 Concurrency / Race：验证 session 隔离、排队和并发安全

## 推荐的最小实现

第一阶段已完成 H5-H7 和 H15，形成可观测的任务状态、快照和跨运行查询基础。

下一阶段优先完成 H9-H12 和 H14，形成可重复、可注入故障的 Agent 测试闭环。

再接入 H13、H16-H18，覆盖真实模型回归、安全边界、预算和并发。

具体实现顺序：

1. Scripted Provider。
2. Event Collector。
3. Scenario Runner。
4. 基础断言。
5. 读取文件、工具失败恢复、多轮对话、用户提问、取消和预算限制等核心场景。

建议目录结构：

```text
internal/harness/
  scripted_provider.go
  scenario.go
  runner.go
  event_collector.go
  fake_clock.go
  faults.go
  assertions.go
  fixtures/
```

## 优先级

```text
P0  运行时状态与可恢复执行
P0  Run Snapshot + Web 状态查询
P0  Scripted Provider + Scenario Runner
P0  Event Collector + Fault Injection
P1  Streaming/Event Contract
P1  Security/Workspace Boundary
P1  Budget/Cost
P1  Concurrency/Race
P2  真实模型回归评测
P2  长任务稳定性与成本基准
```

详细说明见 [`docs/harness/agent-harness-proposals.md`](docs/harness/agent-harness-proposals.md)。

最终目标是：新增一个 Agent 能力时，只需要新增一个场景和几条断言，就能同时验证模型请求、工具行为、用户输出、Trace、Session、取消语义和资源消耗。
