# Skill 评审界面重新设计

## 0. 这份文档解决什么

原有 `cmd/skill-review/web/index.html` 是一个**只读的 Path 诊断页**：假设 `cases.json` / `paths.json` / `runs.json` 已经由外部准备好，页面只负责展示。

但真实的工作流缺了最前面两步——**Skill 从哪来、测试用例怎么攒**。这次把界面重新拆成一条完整的、左侧分栏、模块分工明确的操作链路：

```text
① Skill 管理  ──▶  ② 测试用例构建  ──▶  ③ 评估与对比
  编写 Skill        收集一次真实运行      用例跑 Skill
  生成预设 Path      抽取成 Case/Run       预期路径 vs 实际路径
```

界面风格要求：**朴素**。去掉大色块、渐变、阴影堆叠，改为浅色描边 + 单主色 + 等宽字体标注 ID 的信息型排版。左侧是固定的栏目导航，右侧是当前栏目的工作区。

---

## 1. 模块一：Skill 管理

### 1.1 目标

以「每一个 Skill」为管理单元。一个 Skill 在磁盘上就是这样一个目录结构（对应截图）：

```text
content-expert/
├── bin/            # 可执行脚本（工具实现）
├── references/     # 深度指南，按需加载
└── SKILL.md        # 元数据(frontmatter) + 正文
```

`SKILL.md` 的 frontmatter 已由 `internal/skills/skill.go` 定义：`name` / `description` / `metadata.requires` / `always`。`description` 是唯一的触发信号（既写"做什么"也写"何时用"）。

### 1.2 界面能做什么

- **Skill 列表**：左侧栏目内列出所有 Skill（读 `skills/` 目录），显示 `name`、`description` 摘要、依赖是否满足。
- **上传 / 新建 Skill**：上传一个 Skill 目录（或 zip），或在线新建。
- **在线编写 SKILL.md**：编辑 frontmatter 与正文，保存回磁盘。
- **生成预设 Path**：从 SKILL.md 正文解析出该 Skill 的执行路径（见 1.4）。

### 1.3 意图匹配与 Skill 链路的关系（关键）

szabot **没有"统筹 Skill"**。所有 Skill 的 L1 描述被平铺进 system prompt，**由模型自己**根据各 Skill 的 `description`（"何时用"）判断该用哪个——意图识别正是 LLM 最擅长的事，Skill 之间是**扁平、平级**的，不存在一个大 Skill 去路由/统筹全局。

```text
所有 Skill 的 description（平铺进 system prompt）
        │
   模型自主匹配意图
        │
   命中 content-expert（触发词 @expert:market …）
        │
   读 references/content.md → 执行 content-sim → 校验结果 → 产出报告
```

因此**一条 Skill 的完整 Path = 模型意图匹配命中该 Skill + 该 Skill 自身的完整执行链路**：

```text
match_intent  模型据 description 命中 content-expert
        ↓
[content-expert] 读 references/content.md → 执行 content-sim → 校验结果 → 产出报告
```

Path 最前面的入口节点（`match_intent`）代表的是**模型的意图匹配**，而不是某个统筹 Skill 的执行。之所以保留它，是因为"输入有没有命中正确的 Skill"是评审第一优先级要验证的（对应 `Evaluate` 的 `skill_not_selected`）。

### 1.4 从 SKILL.md 生成预设 Path

Path 不是凭空写的，而是从 SKILL.md 的正文推导。解析规则（启发式，允许人工二次修正）：

| Path 节点来源 | 在 SKILL.md 里的信号 | 生成的 Node |
|---|---|---|
| 入口 / 意图匹配 | frontmatter `description` 里的触发关键词（`@expert:market` 等） | `kind=input`（`match_intent`，模型据 description 命中） |
| 前置动作 | "必须先读取 references/xxx.md" 之类规则 | `kind=validation` / `note` |
| 工具调用 | `## 命令速查` 里的 `bash .../content-sim …` | `kind=tool`，`tool=content-sim` |
| 结果校验 / 注意事项 | "重要规则" 段落 | `kind=validation`，或转成 `note`/`forbidden` |
| 异常处理 | "异常处理" 段落 | `kind=fallback` |
| 输出 | 正文里对结果格式的描述 | `kind=output` |

生成结果就是现有的 `PathDefinition`（`internal/skillreview/review.go`），页面允许人工调整后保存为 `paths.json`。

> 生成器可以先做「关键词/小标题 → 节点」的规则版，够用后再考虑用 LLM 辅助抽取。

---

## 2. 模块二：测试用例构建

### 2.1 核心思路

构建测试用例 ≠ 手写一堆 JSON，而是**收集一次真实运行的过程，再抽取成结构化用例**。

szabot 的运行过程天然是一条 **SSE 事件流**（`internal/channels/web.go` 已经在发）。一次对话会产生一串事件，界面负责把它「录」下来、解析、抽取成我们需要的 `Case`（预期）与 `Run`（实际）。

```text
用户输入
   │  POST /api/send
   ▼
Agent 执行（Runner）
   │  SSE: data: {text, kind, delta, done}
   ▼
事件流（原始 trace）
   │  ← 本模块在这里介入：录制 + 解析
   ▼
抽取 → Case / Run（结构化）
```

### 2.2 SSE 事件流长什么样

现在 web channel 输出的每条事件是：

```json
data: {"text": "...", "kind": "answer|reasoning|tool_call|tool_result", "delta": true, "done": false}
```

runner 侧的 `StreamSink` 已经区分了四类事件，正好对应我们要抽取的节点：

| StreamSink 回调 | 语义 | 抽取成 |
|---|---|---|
| `OnReasoningDelta` | 模型思考过程 | `decision` 节点（意图判断/分支选择的依据） |
| `OnToolCall(call)` | 请求调用某工具（含 name + 参数） | `tool` 节点，`tool=call.Name`，进 `tool_calls` |
| `OnToolResult(call, result)` | 工具执行结果 | `tool` 节点的结果，用于校验 |
| `OnContentDelta` | 面向用户的正文 | `output`（拼成最终答案 `output_text`） |

> **建议**：为了让抽取更准，可以给 SSE 事件补一个 `node_id` / `skill` / `event` 字段（在 `web.go` 的 payload 和 `StreamSink` 里加），这样抽取时不必靠文本猜。这属于「提升可观测性」，是阶段三里明确建议的补埋点。

### 2.3 界面怎么抽取

界面提供两种录入方式：

1. **实时录制**：页面里直接对话（复用 `/api/send` + `/api/stream`），一次运行结束后把捕获到的完整事件流留在页面。
2. **粘贴 / 导入**：把一段已有的 SSE 事件流（或 session log）粘进来。

拿到事件流后，页面做解析和抽取：

```text
原始 SSE 事件流
   │  按 kind 分类、按顺序编号
   ▼
解析出：
   selected_skills   ← 命中的 Skill（从事件里的 skill 字段 / 输入中的触发词）
   actual_path       ← 有序 node 列表
   tool_calls        ← 所有 tool_call 的 name
   execution_chain   ← 有序的 (skill, tool) 步骤
   output_text       ← 拼接的正文
   │
   ▼
人工确认 / 微调后，另存为两份：
   Run       ← 这次「实际发生了什么」（可直接作为 runs.json 的一条）
   Expected  ← 由这次运行「反向」生成一条预期（用户确认这就是正确行为时）
```

关键点：**一次好的运行，既是一条 `Run`，也可以固化成一条 `Case.Expected` 作为基线**。以后 Skill 改动后重新运行，新 `Run` 再跟这条 `Expected` 对比，就能发现回归。

抽取产物直接复用现有结构（无需改数据模型）：`Case` / `Expected` / `Run` / `TraceEvent`（`internal/skillreview/review.go`）。

---

## 3. 模块三：评估与对比

### 3.1 目标

用测试用例（`Case`）去评估某个版本的 Skill，核心是**一条路径的对比**：预期路径 vs 实际路径。

```text
Case.Expected  ─┐
                ├─▶ Evaluate() ─▶ 逐节点对比 ─▶ 失败码 + 指标
Run（实际）     ─┘
```

`Evaluate()` 已经实现了全部对比逻辑与失败码（`skill_not_selected` / `path_mismatch` / `chain_step_missing` / `notice_missed` / `node_missing` / `output_*` 等）。本模块是它的**可视化前端**。

### 3.2 界面怎么呈现对比

以「路径对比」为主视图：

```text
预期路径:  detect_input → load_skill → run_content-sim → verify → output
实际路径:  detect_input → load_skill → run_content-sim → ✗(缺 verify) → output
                                                          ▲
                                                    失败节点高亮
```

- **并排双轨**：上排预期节点，下排实际节点，逐列对齐；不一致的节点标红并给出失败码。
- **指标区**：Skill 命中率 / Path 覆盖率 / 节点通过率 / 链路通过率 / 注意事项通过率 / 输出通过率（都来自 `Metrics`）。
- **失败列表**：每条失败显示 `code` + `message` + `failed_node`。

### 3.3 版本对比（回归，可后续做）

同一个 Case 在 Skill v1 / v2 各跑一次，把两次 `Run` 与同一 `Expected` 对比，标出「新修复 / 新回归 / 保持」。这对应计划文档的 M4，本次先留栏目占位。

---

## 4. 界面布局

左侧固定栏目导航，右侧工作区随栏目切换。朴素、信息型、无花哨装饰。

```text
┌──────────────┬───────────────────────────────────────────────┐
│  Skill Review │  [当前栏目工作区]                              │
│               │                                               │
│ ● Skill 管理   │   （随左侧选择切换）                            │
│ ○ 测试用例      │                                               │
│ ○ 评估对比      │                                               │
│ ○ 回归(规划)    │                                               │
│               │                                               │
│ ── 上下文 ──   │                                               │
│ 当前 Skill: …  │                                               │
│ 版本: …        │                                               │
└──────────────┴───────────────────────────────────────────────┘
```

栏目与模块一一对应，分工明确：

| 左侧栏目 | 对应模块 | 主要动作 |
|---|---|---|
| Skill 管理 | 模块一 | 列表 / 上传 / 编辑 SKILL.md / 生成 Path |
| 测试用例 | 模块二 | 录制或粘贴 SSE → 解析 → 抽取 Case/Run |
| 评估对比 | 模块三 | 选 Case + Run → 路径对比 + 指标 |
| 回归（规划） | 模块三扩展 | 多版本 Run 对比，标新增失败 |

---

## 5. 数据与接口（复用为主）

数据结构全部复用 `internal/skillreview/`，不新增模型：

- Skill：`internal/skills`（`Skill` / `Meta`）
- Path：`PathDefinition` / `NodeDefinition`
- 用例：`Case` / `Expected`
- 运行：`Run` / `TraceEvent`
- 评估：`Evaluate` → `Report` / `Metrics` / `Failure`

后端接口在现有 `cmd/skill-review/server.go` 基础上扩展（本文档只定义契约，实现分期落地）：

| 接口 | 方法 | 作用 |
|---|---|---|
| `/api/skills` | GET | 列出 Skill（name/description/requires） |
| `/api/skill?name=` | GET/PUT | 读取 / 保存某个 SKILL.md |
| `/api/skill/paths` | POST | 从 SKILL.md 生成预设 Path |
| `/api/extract` | POST | 传入 SSE 事件流，返回抽取的 Case/Run |
| `/api/data` | GET | 现有：modules + report + paths（评估结果） |

> 当前这次先落地**前端界面骨架**（三栏目、朴素风格、路径对比视图），后端接口按上表逐步补。没有后端时，界面用内存态 / 本地文件占位，保持可交互。

---

## 6. 落地顺序

1. 前端重构：左侧栏目导航 + 三个工作区（本次）。
2. 评估对比视图：预期/实际双轨路径对比（本次，复用 `/api/data`）。
3. Skill 管理接口：`/api/skills`、`/api/skill`。
4. Path 生成器：`/api/skill/paths`（规则版）。
5. SSE 抽取器：`/api/extract`，并给 SSE 事件补 `node_id`/`skill` 字段。
6. 回归对比：多版本 Run diff。
