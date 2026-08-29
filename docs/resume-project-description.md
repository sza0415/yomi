# Yomi 项目简历描述

> 本文只整理当前仓库已经落地的能力，适合作为简历、项目介绍和面试准备的统一素材。项目代码仍保留 `szabot` 包路径、命令和环境变量作为兼容标识。

## 项目定位

**Yomi** 是一个使用 Go 实现的可扩展 Agent Runtime，核心贡献是长上下文 Agent 的分层 Context Engineering。项目以
`MessageBus → AgentLoop → Runner → Provider` 为最小闭环，把 Channel、Tool、Provider、Skill 作为边缘扩展点，并在不破坏原始会话和审计事实的前提下按上下文压力分层处理工具结果、历史消息和阶段摘要。

Yomi 面向本地开发和 Skill 评审场景，提供 CLI/Web 交互、受控工作区工具、Docker 执行沙盒、会话恢复、流式事件和可查询 Trace，能够观察一次 Agent Run 的输入、推理、工具调用、上下文压缩策略、工具结果和最终输出。

## 简历项目经历（推荐版）

**Yomi：可扩展 Go Agent Runtime**  | Go、HTTP/SSE、JSONL、Docker

- 设计并实现 `MessageBus → AgentLoop → Runner` 为核心循环，将 Channel、Tool、Provider、Skill 作为循环扩展能力接入；支持同 Session FIFO 串行、跨 Session 并行。
- 设计并实现工具白名单注册中心（Registry），统一管理 Agent 可用工具，拒绝未注册的工具调用；工具定义按名称字母序稳定导出，以保持请求前缀一致并利于前缀缓存。
内置以下工具：`文件读写/编辑``
- 文件检索：glob、grep
- 网络访问：web_fetch、基于 Tavily 的 web_search
- 任务状态：todo_write
- 用户交互：ask_user_question
- 大型结果读取：artifact_read
启动 Docker Sandbox 后，按配置注册 bash 和 python 执行工具。ask_user_question 复用消息总线的双向通道，使 Agent 能跨 CLI、Web 等不同 Channel 主动向用户提问，并支持携带可选项、等待用户回答后继续执行。
- 实现带最大工具轮次约束的多轮 Function Calling 引擎：通过工具白名单 `Registry` 校验名称与参数 Schema，结合 `PermissionGate` 控制高风险执行，工具定义按字母序稳定输出以保持请求前缀稳定；支持模型一次返回多个 Tool Call、错误回灌、有限重试、超时、取消和模型/工具预算控制。
- 将 reasoning 增量、assistant 正文、Tool Call、Tool Result、用户提问和生命周期状态统一为结构化事件流；支持 OpenAI-compatible Provider 的 SSE 流式输出、AG-UI 事件翻译、JSONL Trace 持久化和 Web 状态查询，便于分类渲染、排障与评审。

- 设计并实现分层 Context 压缩机制：统一评估 system、Conversation、Archive、工具定义、工具结果和 output reserve 的 token 预算；按压力执行大工具结果 Artifact 外置、工具专属 preview、ArchiveRecord 和 rolling summary，同时保留原始 Conversation/Trace，通过 `artifact_read` 恢复细节，并以 `context.strategy.applied` 和按 Model Step 聚合的 Web Trace 解释每次压缩决策。
- 补齐运行时状态与扩展链路：Agent 状态栏、Session JSONL、Run Snapshot、Skill L1/L2/L3 渐进式加载和 Skill Path 评审；同时实现运行时与安全 Harness，覆盖状态机、显式取消、重试、workspace 权限边界、Docker 默认禁网和资源限制。

以及 Run/Model/Tool 三层状态机。
## 一页精简版

使用 Go 从零实现可扩展 Agent Runtime Yomi，主攻长上下文 Agent 的分层 Context 压缩：统一预算评估，按工具结果 Artifact 外置、工具专属 preview、ArchiveRecord、rolling summary 逐级处理上下文压力，同时保留原始 Conversation/Trace 并通过 `artifact_read` 恢复细节。以 `MessageBus → AgentLoop → Runner → Provider` 为核心循环，实现多轮 Function Calling、SSE 事件、状态栏、主动提问、运行时 Harness、安全边界 Harness、HTTP/SSE Web、Skill 三层加载和 Docker 默认禁网沙盒。

## 技术亮点展开

### 1. 极简核心循环与扩展边界

```text
Channel → MessageBus → AgentLoop → Runner → Provider
              ↑                         │
              └──── Outbound events ────┘
```

- **Channel**：把 CLI、Web 或其他平台消息翻译成统一的 `InboundMessage`/`OutboundMessage`。
- **MessageBus**：只承载入站、出站消息，不理解平台和模型。
- **AgentLoop**：按 Session 恢复历史、构造上下文、管理 Run 生命周期并路由输出。
- **Runner**：执行模型请求、工具循环、状态注入、重试、预算和取消。
- **Provider**：统一 Echo 与 OpenAI-compatible 协议，兼容 DeepSeek、OpenAI、Moonshot、Ollama 等服务。

新增接入点优先在装配层注册，不需要侵入 Loop/Runner 的主流程，保持核心可测试、可替换。

### 2. 多轮 Tool Calling 与白名单 Registry

`Tool` 统一暴露 `Name`、`Description`、JSON Schema 和 `Execute`；`Registry` 是显式 allowlist，拒绝空名称、重复名称、nil 工具和非法 Schema。Runner 每轮将稳定排序后的定义发给 Provider，收到 Tool Call 后校验 ID/名称，执行工具并把 `role=tool` 结果关联回 `tool_call_id`，直到模型返回最终正文或达到最大工具轮数。

工具错误会以结果消息回灌模型，模型可以根据错误自行修正；Provider 错误和实现了 `RetryClassifier` 的工具支持有限次数重试与指数退避。Run 还可限制模型调用数、工具调用数和 token 使用量。

### 3. 状态栏与主动提问

`StatusProvider` 将状态源与 Runner 解耦。当前由 `todo_write` 按 Session 保存任务清单，渲染为“任务清单 2/5 已完成”和 `[x]`、`[~]`、`[ ]`、`[-]` 状态标记。每次 `chatOnce` 前重新读取并把状态作为临时 user 消息追加在上下文末尾：不写入 Conversation、不改变固定 System Prompt，空清单不注入，动态信息不会破坏稳定前缀。

`ask_user_question` 不直接依赖 stdin 或 Web API，而是通过 Context 注入的 `Asker` 调用 Loop。Loop 登记 pending question，把带 options 的 `KindQuestion` 出站消息交给 Channel；下一条同 Session 入站消息被识别为回答并唤醒工具。这样同一套交互语义同时覆盖 CLI、Web 卡片和未来 Channel。

### 4. 统一事件流、持久化与恢复

Runner 的 `StreamSink` 统一汇报模型请求、reasoning、正文增量、Tool Call、Tool Result、用户提问、状态变化和耗时。Loop 将事件映射为带 `RunID`、`AgentID`、序号和 Kind 的 OutboundMessage；Web 侧进一步翻译为 `TEXT_MESSAGE_*`、`REASONING_MESSAGE_*`、`TOOL_CALL_*`、`TOOL_RESULT`、`RUN_*` 等 AG-UI/SSE 事件。

- **Conversation**：按 Session 一个 JSONL 文件，仅保存成功 Run 的用户消息与最终 assistant 正文，供下一轮模型上下文使用；reasoning、Tool Call/Result 等内部消息只进入 Trace。
- **Trace**：按 Run 一个 JSONL 文件，保存 system prompt、模型请求快照、reasoning、Tool Call/Result、错误、usage 和终态，不回放给模型。
- **Run Snapshot**：记录排队、运行、等待用户、取消和完成等状态；进程重启时将遗留运行标记为 interrupted，便于 Web 查询和后续恢复策略接入。
- **ContextManager**：统一评估消息、工具定义和输出预留的 token 预算；大工具结果进入 Artifact preview，历史超限时生成 rolling summary 和独立 ArchiveRecord，原始历史仍保留在存储中。

### 5. 分层 Context Engineering

Context 压缩不是把全部历史一次性压成一段摘要，而是按成本从低到高、影响从局部到全局逐层处理 WorkingContext：

```text
统一预算评估
  -> L1：大工具结果 Artifact 外置 + 可读 preview
  -> 工具专属 preview：web_search 保留摘要/标题/URL，web_fetch 保留标题/来源
  -> L4：已完成历史生成独立 ArchiveRecord
  -> L5：rolling summary + 最近消息兜底
```

- **预算口径统一**：同时计算 system prompt、Conversation、Archive、工具定义、当前 tool result 和 output reserve，按候选消息加入后的 token 结果决策。
- **信息与上下文解耦**：Artifact 保存大结果原文和元数据，WorkingContext 只放 preview/reference；模型可用 `artifact_read` 按字节范围取回 Session 自己的原文。
- **摘要不覆盖原始事实**：Conversation 和 Trace 保留原始主线与完整运行事实；rolling summary 通过覆盖游标推进，ArchiveRecord 记录 FACTS、CONSTRAINTS、DECISIONS、UNFINISHED、FAILURES、SOURCES 等任务演进信息。
- **策略可观测**：每次压缩记录 `layer`、`action`、`trigger`、`attempted_layers`、`tokens_before/after`、`reversible` 和来源 ID；Web Trace 按 Model Step 展示压缩策略与模型/工具事件的关系。

当前已落地 Artifact 外置、工具专属 preview、Archive 持久化和 rolling summary；L2 噪声删除、L3 Provider API 上下文编辑、Artifact 生命周期治理和选择性 Archive 检索仍属于后续演进方向。

### 6. Skill、工作区与沙盒

Skill Loader 扫描 workspace（workspace 优先覆盖 builtin），将 `name + description + 相对路径` 作为 L1 常驻摘要；普通 Skill 的 L2 正文由 Agent 用 `read_file` 按需读取，L3 references/scripts/assets 由正文引导继续读取或执行；`always=true` 的 Skill 可在依赖满足时常驻正文，缺失依赖会在摘要中标记为 unavailable。

文件工具拒绝绝对路径和 `..` 路径穿越，读/编辑工具额外防止符号链接逃逸。高风险工具经 PermissionGate 分类审批。Bash/Python 在每次调用时创建临时 Docker 容器，默认只读根文件系统、独立 `/tmp`、30 秒超时、资源上限和 `--network none`；workspace 以 `/work` 挂载，因此仍需配合最小工作区、备份和代码审查。

### 7. Skill 评审与真实运行链路

`cmd/skill-review` 可读取真实 Run Trace，将 Skill 执行过程映射为可评审的 Path/Node/Branch：区分 Skill 选择、引用读取、决策分支、工具执行、校验、兜底和最终输出，支持规则抽取与 LLM 抽取、`PATH.json` 缓存、失败原因分类及 Markdown/JSON 报告。这样评审对象从“最终答案好不好”扩展到“是否命中正确 Skill、是否走对分支、是否调用正确工具、是否满足输出约束”。

### 8. Harness：把 Agent 从“能跑”变成“可控地跑”

#### 运行时 Harness

运行时 Harness 包在 AgentLoop/Runner 外围，解决 Agent 执行过程中“如何感知状态、如何交互、如何停止、如何恢复”的问题：

- **状态与生命周期**：Run、Model、Tool 三层状态机，限制非法状态跳转；Run Snapshot 持久化排队、运行、等待用户、取消和终态。
- **交互与流式**：状态栏每轮注入最新 todo，`ask_user_question` 支持暂停/回答/继续，reasoning、正文、Tool Call/Result 通过 SSE/AG-UI 实时推送；完整 reasoning、assistant 消息和工具事实旁路落 Trace。
- **停止与恢复**：Web 端显式取消向下游 Runner/Provider 传播 context；SSE 断连和重连不影响 Run；进程重启时识别并标记遗留 Run。
- **故障收敛**：区分 Provider、工具、超时、取消和预算错误，按策略有限重试并保证 pending question、running registry 和 Run 终态清理。

#### 安全与可靠性边界 Harness

安全 Harness 关注“工具能做什么、失败时是否越界、资源耗尽时是否仍可控”：

- **路径边界**：拒绝绝对路径、`..` 穿越和读/编辑工具的 workspace 外符号链接逃逸；工具只接收注册表中的名称和 JSON Schema。
- **权限边界**：PermissionGate 对只读、写文件、联网、Bash/Python 等能力进行风险分类；拒绝结果进入 Tool Result 和 Trace，模型不能绕过注册表直接执行本机函数。
- **执行边界**：Docker 临时容器默认 `--network none`，只读根文件系统、独立 `/tmp`、超时、内存、CPU、PID 和输出上限；宿主 workspace 挂载风险在文案和运行说明中明确暴露。
- **预算与可观测性**：RunBudget 限制输入/输出/总 token、模型调用和工具调用；Trace 记录实际 usage、错误、耗时和终态，支持后续 Deterministic Provider、Scenario Runner 与安全回归集接入。

## 面试讲解主线

1. **先讲核心问题**：长上下文 Agent 为什么会因为工具结果和历史累积而失控，为什么需要 WorkingContext、Conversation、Trace、Artifact、Archive 分层。
2. **重点讲压缩策略**：如何用统一 token 预算在工具结果进入模型前做 Artifact preview，再用 Archive 和 rolling summary 处理历史；如何保证原始事实可恢复、压缩失败可回退。
3. **再讲核心循环**：一次用户请求如何经过 `MessageBus → AgentLoop → Runner → Provider`，Tool Calling 如何受 Registry、轮次和预算约束。
4. **最后讲 Harness**：运行时 Harness 如何处理状态、提问、流式、断连不取消、显式取消和恢复；安全边界 Harness 如何处理路径、权限、Docker 资源限制、预算和 Trace 审计。

## 可核验的运行入口

```bash
go test ./...
go run ./cmd/szabot                         # CLI + Echo Provider
SZABOT_WEB=1 go run ./cmd/szabot            # HTTP + SSE Web
go run ./cmd/skill-review -h                # Skill 评审入口
go run ./cmd/overview -addr 127.0.0.1:8091  # 项目地图与文档工作台
```

## 表述边界

- 可以表述为“已实现”：统一 ContextBudget、Artifact 外置与 `artifact_read`、工具专属 preview、ArchiveRecord、rolling summary、策略 Trace、按 Model Step 的 Trace 分组，以及多轮 Tool Calling、白名单 Registry、流式事件、状态栏、主动提问、Session JSONL、Run Snapshot、Web 显式取消、运行时状态机、Provider/Tool 重试、权限门、workspace 边界、Docker 沙盒、CLI/Web、Skill L1/L2/L3 和 Skill Path 评审。
- 不宜表述为“已完成”：L2 自动噪声删除、L3 Provider API 上下文编辑、Artifact 生命周期治理、Archive 选择性检索、完整越权攻击集、审批审计事件、Deterministic Provider/Scenario Runner、真实模型回归集、断点续跑和生产级公网安全防护；这些仍在设计或 Harness 路线图中。
- Web 当前面向可信本机环境；`web_fetch` 仍需额外防护 SSRF，Docker workspace 挂载也不是不可变沙盒。
