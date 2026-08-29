<p align="center">
  <img src="assets/yomi-logo.svg" alt="yomi" width="620">
</p>

# yomi

一个用 Go 实现的轻量 Agent 框架。yomi 先用少量角色组成一条稳定的核心循环，再把接入方式、工具、技能和模型能力放到循环边缘扩展。

> 项目名称统一为 **yomi**。当前代码入口 `cmd/szabot`、环境变量 `SZABOT_*` 以及部分存储路径仍保留早期技术标识，以兼容现有用法。
## 目录结构

```
szabot/
├── cmd/
│   ├── szabot/             # CLI 入口（main.go：只做装配）
│   └── skill-review/       # Skill 评审入口（独立工具）
├── internal/
│   ├── bus/                # 消息总线（系统中枢）
│   ├── agent/              # 核心循环
│   │   ├── loop.go         #   外层：消费 bus、协调上下文
│   │   └── runner.go       #   内层：跟 LLM 来回打交道
│   ├── channels/           # 通道（平台翻译官）
│   │   └── cli.go          #   stdin/stdout 实现
│   ├── skills/             # 技能加载与运行支持
│   ├── skillreview/        # Skill 评审内核
│   └── providers/          # LLM 提供商
│       ├── provider.go              #   统一接口
│       ├── echo.go                  #   假实现，零依赖验证链路
│       └── openai_compatible.go     #   OpenAI 兼容（DeepSeek/OpenAI/Moonshot/Ollama...）
└── go.mod
```

## 5 个核心角色

| 角色 | 朝向 | 职责 |
|---|---|---|
| **Channel** | 朝外 | 把平台原生消息 ⇄ 统一 `InboundMessage` / `OutboundMessage` |
| **MessageBus** | 中枢 | 用 Go channel 承载入站和出站消息 |
| **AgentLoop** | 中间 | 从 bus 接消息、加载 session、协调一次运行并推回 bus |
| **AgentRunner** | 朝内 | 跟 Provider 对话，驱动多轮 tool calling 循环 |
| **Provider** | 朝内 | 提供统一的 Echo 与 OpenAI-compatible LLM 接口 |

这五个角色构成边界：Channel 负责世界接入，Provider 负责模型接入，中间的循环只负责消息编排和一次运行，不承载具体业务。

## Agent 核心循环

核心循环按下面的顺序理解最容易：

```text
Channel -> MessageBus -> AgentLoop -> AgentRunner -> Provider
             ^                         |
             └──── OutboundMessage ───┘
```

### MessageBus：消息总线

MessageBus 是系统中枢，提供入站和出站两条队列。Channel 把平台消息翻译成统一的入站消息并写入 bus；AgentLoop 消费入站消息，再把结果写回出站队列。bus 不理解 CLI、Web 或具体模型，因此可以在不改核心循环的情况下替换接入端。

### AgentLoop：运行编排

AgentLoop 负责一次 Run 的外层生命周期：消费 bus 消息、按 SessionID 恢复会话、准备上下文、调用 Runner，并将最终回复或流式事件推回 bus。它协调运行，但不实现模型协议、工具细节或平台协议。

### AgentRunner：模型与工具循环

AgentRunner 是内层执行器，负责把 system prompt、Conversation 和本轮输入交给 Provider；收到 tool call 后执行工具、把结果追加回请求，直到得到最终 assistant 正文或发生错误/取消。多轮 tool calling、取消、重试和运行状态都属于这一层的执行语义。

## Agent 框架扩展

核心循环稳定后，新的能力优先通过四类扩展接入。

### Channel 接入

- **CLI**：stdin/stdout，SessionID 固定为 `cli:local`。
- **Web**：HTTP + SSE；`POST /api/send` 提交消息，`GET /api/stream?session=...` 接收流式事件。浏览器用 `localStorage` 保存 SessionID。

Web 没有认证和访问控制，只适合可信的本机环境。启用方式：

```bash
export SZABOT_WEB=1
export SZABOT_WEB_ADDR=127.0.0.1:8080
go run ./cmd/szabot
```

### Tool 接入

#### 工具箱

| 工具 | 说明 | 依赖 |
|---|---|---|
| `read_file` / `write_file` / `edit_file` | 读取、覆盖写和精确替换工作区文本文件 | 无 |
| `list_dir` / `glob` / `grep` | 浏览目录、按名称查找、按正则搜索内容 | 无 |
| `web_fetch` | 读取 HTTP/HTTPS 页面并提取正文 | 无 |
| `todo_write` | 按 Session 维护任务清单和进度 | 无 |
| `ask_user_question` | 通过当前 Channel 向用户提问并等待回答 | 无 |
| `web_search` | 通过 Tavily 搜索互联网 | `TAVILY_API_KEY` |
| `bash` / `python` | 在临时 Docker 容器中执行命令或代码 | Docker + `SZABOT_SANDBOX=1` |

#### 工具注册条件

`read_file`、`write_file`、`edit_file`、`list_dir`、`glob`、`grep`、`web_fetch`、
`todo_write` 和 `ask_user_question` 默认注册。`web_search` 仅在设置
`TAVILY_API_KEY` 后注册；`bash` 和 `python` 仅在显式启用 Docker 沙盒且 Docker
可用时注册：

```bash
export TAVILY_API_KEY=tvly-xxxx
```

> `web_fetch` 当前只校验 URL 为 HTTP/HTTPS，没有阻止访问 localhost、私网地址、云元数据地址或重定向后的内网目标。因此不适合直接开放给不可信用户。

#### Docker 沙盒

```bash
export SZABOT_SANDBOX=1

# 可选
# export SZABOT_SANDBOX_NETWORK=1       # 允许容器联网，默认关闭
# export SZABOT_PYTHON_IMAGE=python:3.12-slim
# export SZABOT_BASH_IMAGE=debian:stable-slim
# export SZABOT_SANDBOX_TMP_SIZE=512m   # 默认 64m
```

未开启 `SZABOT_SANDBOX`、找不到 Docker CLI 或 Docker daemon 未运行时，yomi 会跳过 `bash` 和 `python`，其他工具仍可使用。每次执行都会创建临时容器，默认限制为 30 秒、512 MB 内存、1 个 CPU、256 个进程和 64 KiB 返回输出；根文件系统只读，`/tmp` 使用临时文件系统，网络默认关闭。

> Docker 沙盒不是只读工作区：宿主 workspace 会以**读写方式**挂载到容器的 `/work`。容器内命令可以创建、修改或删除工作区文件，这些变化会直接影响宿主机。沙盒主要隔离工作区之外的文件系统、网络和资源，不能替代权限控制、备份或代码审查。

文件工具拒绝绝对路径和 `..` 路径穿越；`read_file`、`edit_file` 还会拒绝解析后逃出 workspace 的符号链接。当前 `write_file` 只进行词法路径检查，若目标或父目录是指向 workspace 外部的符号链接，仍存在越界写风险。因此不要在工作区放置指向敏感位置的可写符号链接，也不要把工具能力开放给不可信用户。

Docker 安装、镜像选择、完整限制和安全边界见 [`docs/tools-and-sandbox.md`](docs/tools-and-sandbox.md)。

### Skill 加载

Skill 从 workspace 发现，并在 Agent 运行时按需加入模型上下文。默认关闭：

```bash
export SZABOT_SKILLS=off
export SZABOT_SKILLS=auto
export SZABOT_SKILLS=kbcli,github
```

详细定义和加载路径见 [`docs/skill-execution-path-review.md`](docs/skill-execution-path-review.md)。`cmd/skill-review` 是独立的离线评审入口，不参与主 Agent 链路；设计见 [`docs/skill-review-plan.md`](docs/skill-review-plan.md)。

### Provider 接入

Provider 统一 Echo（零依赖验证）和 OpenAI-compatible 实现，后者可复用 DeepSeek、OpenAI、Moonshot、Ollama 等服务。主程序当前通过环境变量装配 Echo 或 DeepSeek：

```bash
export SZABOT_PROVIDER=deepseek
export DEEPSEEK_API_KEY=sk-xxxxxxxx
export DEEPSEEK_MODEL=deepseek-v4-pro
go run ./cmd/szabot
```

未设置 Provider 时使用 Echo。OpenAI-compatible Provider 支持非流式响应和 SSE 增量中的 `reasoning_content`，并可将思考过程写入 Trace。当前 `buildProvider` 仍会把 API key 明文打印到控制台，仅建议本地调试使用。

### 快速开始

不设置任何环境变量即可运行零依赖的 Echo Provider：

```bash
go run ./cmd/szabot
```

输入一条消息后会收到 `echo:` 回复；按 Ctrl+C 退出。

## 能力扩展

### 上下文管理

Conversation 是供后续请求使用的主线历史，按 SessionID 持久化为 JSONL；只有 Run 成功完成后，才追加本轮 user 和最终 assistant。超过预算时，较早消息由当前 Provider 压缩为 rolling summary，再发送摘要、最近消息和当前输入；原始历史不删除。

```bash
export SZABOT_MAX_CONTEXT_TOKENS=8000
export SZABOT_CONTEXT_RECENT_MESSAGES=10
export SZABOT_TOOL_RESULT_MAX_TOKENS=1200
export SZABOT_CONTEXT_OUTPUT_RESERVE_TOKENS=1024
export SZABOT_SESSION_DIR=/data/yomi-sessions
```

默认目录为启动工作目录下的 `sessionlogs/`：

```text
sessionlogs/
├── conversations/   # 会话主线
├── archives/        # 分阶段压缩检查点
├── artifacts/       # 超大工具结果的原文和元数据
└── traces/          # Run 完整轨迹
```

单次 Run 内，超过 `SZABOT_TOOL_RESULT_MAX_TOKENS` 或导致上下文接近上限的工具结果会
写入 `artifacts/`，模型只收到 preview 和 `artifact_id`。模型可以调用
`artifact_read` 按字节范围读取当前 Session 自己的原始结果。完整工具结果仍写入 Trace，
Artifact 不会替代 Conversation 的主线历史。

当历史被 rolling summary 压缩时，yomi 会在 `archives/` 追加独立的 ArchiveRecord，保存
覆盖范围、Run 和摘要内容，便于回溯任务演进；归档不会被默认全部回放给模型。

重置 Conversation 前先停止 yomi，再删除对应文件。当前锁只覆盖单个进程，不要让多个进程共享同一个 session 目录。

### Trace 建设

Trace 不会回放给模型，按 Run 保存 JSONL 事件：Run 生命周期、system prompt、实际模型请求、reasoning、assistant 消息、工具参数与结果、错误、耗时和 usage。Trace 含未脱敏原文，当前没有自动轮转、保留期或脱敏机制。完整格式和写入时机见 [`docs/conversation-and-trace.md`](docs/conversation-and-trace.md)。


## Harness

Harness 用来验证 Agent 在运行时、故障、安全和资源边界下仍然可控。详细设计与里程碑见 [`README_2.md`](README_2.md)。

### 运行时 Harness

- [x] 状态栏、用户提问、显式取消、流式输出和 Trace 持久化
- [x] Run / Model / Tool 三层状态、Run Snapshot 和 Web 状态查询
- [x] Provider 与工具错误分类、有限次数重试和指数退避

### 安全与权限 Harness

- [x] 文件工具 workspace 边界、路径穿越和越界 symlink 防护
- [x] Bash/Python Docker 沙盒：资源限制、临时文件系统和默认禁网
- [x] 宿主侧 PermissionGate：只读工具自动放行，高风险工具请求批准
- [ ] 完整越权攻击集、审批审计事件和 workspace 子目录级策略

安全模式下的审批提供 `Allow once`、`Allow always` 和 `Deny`。选择
`Allow always` 后，该工具在当前进程内不再重复询问；重启 yomi 后会恢复为需要审批。

```bash
export SZABOT_PERMISSION_MODE=safe
export SZABOT_PERMISSION_MODE=workspace-write
export SZABOT_PERMISSION_MODE=full
```

这些模式只控制宿主侧审批，不会扩大 workspace 路径边界，也不会自动开启 Docker 网络。

### 测试与评测 Harness

- [x] Runner、Provider、工具和取消流程的确定性单元测试
- [ ] Deterministic Provider、Scenario Runner、事件契约和真实任务回归集

```bash
go test ./...
```




## 目录结构

```text
szabot/
├── cmd/szabot/         # 主程序入口，只做装配
├── cmd/skill-review/   # Skill 独立评审入口
├── internal/bus/       # 消息总线
├── internal/agent/     # loop.go / runner.go
├── internal/channels/  # CLI、Web 等通道
├── internal/skills/    # Skill 加载与运行支持
├── internal/providers/ # Provider 接口与实现
└── go.mod
```

## 设计宪法

1. **Core stays small**：新功能挂在 channel / tool / provider / skill 边上，不把业务塞进核心循环。
2. **Less structure, more intelligence**：第二个实现出现时再抽接口。
3. **Prefer duplication over premature abstraction**：不写 `BaseChannel` 这类父类。
4. **Explicit over magical**：可配置项都出现在 config struct 中。

## 项目地图

Overview 会读取 workspace 的 `skills/` 和 `docs/`，展示架构、消息流、工具和文档索引，不执行 Skill，也不依赖 Docker：

```bash
go run ./cmd/overview -addr 127.0.0.1:8091 -workspace .
```

访问 `http://127.0.0.1:8091`。Overview 没有认证，只应在可信本机环境使用。

## 路线图

已完成：M1 项目骨架、M2 MessageBus、M3 AgentLoop + AgentRunner、M4 CLI、M5 EchoProvider、M6 OpenAI-compatible Provider、M8 Session 存储、M9 Tool 接口、M10 多轮 tool calling、M10.5 Docker 工具沙盒、M10.6 通用工具、M11 HTTP + SSE Web。

待办：M7 配置加载（`~/.szabot/config.json`）、M12 长期记忆（`MEMORY.md`）。

## 已知问题

`cmd/szabot/main.go` 的 `buildProvider` 会用 `DEBUG:` 打印 `DEEPSEEK_API_KEY` 明文。上线前应移除这些日志。
