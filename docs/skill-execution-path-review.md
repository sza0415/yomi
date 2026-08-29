# Skill 执行路径评审 —— 技术汇总

> 本文从「需求背景 → 设计难点 → 整体架构」三个维度，对 szabot 中 **Skill 执行路径的评审能力**做技术层面的梳理。对应代码：`internal/skills/`、`internal/agent/`、`internal/skillreview/`、`cmd/skill-review/`；设计文档见 `docs/skill-review-plan.md`、`docs/skill-review-ui-redesign.md`。

---

## 一、需求背景

### 1.1 问题：评审止于「总分」

传统 Skill 评审往往只给一个总分，带来一个根本缺陷——**无法回答定位类问题**：

- 哪条执行路径被触发；
- 哪个分支 / 节点执行错误；
- 实际结果与预期结果差异在哪里；
- 修改 Skill 后是否真的修复了问题；
- 修改是否破坏了其它已通过场景。

要回答这些问题，评审必须从「结果好坏」下沉到「路径对不对」，把一次用户输入转成一份**可检查的评审合同**。

### 1.2 目标：一条可复现的评审闭环

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                            Skill 执行路径评审闭环                               │
│                                                                              │
│   ① 解析 Skill 集合     ② 构建用户输入测试集     ③ 检查命中 Skill 与完整链路    │
│   (抽取 Path/Node)      (真实输入 + 预期合同)      (预期 skills + 执行链路)     │
│         │                      │                           │                │
│         └──────────┬───────────┴───────────────┬───────────┘                │
│                    ▼                           ▼                            │
│         ④ 检查注意事项与最终输出      ⑤ 定向修改（按失败节点定位）               │
│         (notices + output 断言)              │                               │
│                    │                         │                               │
│                    └─────────────┬───────────┘                               │
│                                  ▼                                           │
│                          ⑥ 全量回归（防破坏已通过场景）                          │
│                                  │                                           │
│                                  ▼                                           │
│                    ┌─────────────────────────┐                               │
│                    │  生成评审报告（可版本对比）│                               │
│                    └─────────────────────────┘                               │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 1.3 被评审的对象：szabot 的 Skill 执行模型

评审的「执行路径」建立在 szabot 自身的 Skill 运行机制之上，这是理解整套设计的前提。

**（1）渐进式披露：三层知识按访问频率分层**

```text
进程启动（一次性，常驻）                      任务触发（运行时，按需）
─────────────────────                       ─────────────────────

  Loader.List() 扫描 skills/                     LLM 据 description 命中某 Skill
        │                                              │
        ▼                                              ▼
  ┌───────────────────────────────┐          ┌─────────────────────────────┐
  │  L1 元数据                     │          │  agent 用 read_file 读取      │
  │  name + description + 相对路径 │ ───────▶ │  (L1 给出的 workspace 相对路径) │
  │  · 每个 skill 仅几十 token      │          └──────────────┬──────────────┘
  │  · 常驻 system prompt          │                         ▼
  │  · 固定不变 → KV Cache 友好     │          ┌─────────────────────────────┐
  └───────────────────────────────┘          │  L2 SKILL.md 正文             │
                                             │  (核心流程，按需读入 context)   │
  ┌───────────────────────────────┐          └──────────────┬──────────────┘
  │  always=true 技能的正文         │                         ▼
  │  (剥离 frontmatter 后常驻)      │          ┌─────────────────────────────┐
  └───────────────────────────────┘          │  L3 references/ · 脚本 · 资产  │
                                             │  (正文引导下自行决定读/执行)     │
                                             └─────────────────────────────┘
```

> 关键约束：`read_file` 工具被限制在 workspace 内且只接受相对路径（`internal/tools/read_file.go`），因此 L1 摘要里给出的路径必须是 workspace 相对路径，agent 才能用现成工具读到，无需新增专门 load 工具。

**（2）无「统筹 Skill」：意图由模型自主匹配**

所有 Skill 的 `description` 被平铺进 system prompt，由模型根据「何时用」自行判断该用哪个。Skill 之间**扁平、平级**，不存在一个大 Skill 去路由全局。

```text
所有 Skill 的 description（平铺进 system prompt）
        │
        ▼
模型自主匹配意图（intent 识别 = LLM 最擅长的）
        │
        ▼
命中某 Skill（如 szabot-data-query）
        │
        ▼
读 SKILL.md → 读 references → 调工具 → 校验 → 产出结果
```

因此一条 Skill 的完整 Path 可定义为：

```text
match_intent（模型据 description 命中该 Skill） + 该 Skill 自身的完整执行链路
```

**（3）真实 Skill 远比「一条直线」复杂**

以 `skills/szabot-data-query/SKILL.md` 为例，单个 Skill 内部就包含多品类分流、双链路取数、铁律与兜底；叠加 `skills/_router.SKILL.md` 的跨 Skill 强制组合，共同构成评审需要覆盖的「执行路径」的真实复杂度：

```text
                        用户输入："查一下《逐玉》的播放量"
                                        │
                                        ▼
              ┌─────────────────────────────────────────────┐
              │ 意图路由（_router.SKILL.md §1 路由速查表）      │
              │ 播放数据 → szabot-copilot + szabot-data-query  │
              │ （§2 强制组合：缺任一 skill 视为执行错误）       │
              └─────────────────────┬───────────────────────┘
                                    ▼
        ┌──────────────────────────────────────────────────────────┐
        │ szabot-data-query 内部「品类门禁 → 执行器分流」双链路         │
        │                                                          │
        │  电视剧播放 ──▶ kb-recall metric 段 ──▶ kbcli kb-search --sql │
        │  电视剧预算/综艺/动漫 ──▶ references + schema ──▶ mcp_exec_sql │
        │                                                          │
        │  铁律1：md_doc 可用时禁止翻老 schema                        │
        │  铁律2：其他品类禁止走 kb-recall/kbcli kb-search             │
        │  兜底：md_doc 不可用才回落 references + mcp_exec_sql        │
        └──────────────────────────────────────────────────────────┘
```

---

## 二、设计难点

### 2.1 执行路径本质不可观测

Agent 由 LLM 驱动，内部的「意图判断、分支选择、工具调用、结果校验」默认是黑盒。评审要断言路径，必须先解决**可观测性**——把运行时的中间事件暴露出来，而不是只留最终文本。

**解决落点**：`internal/agent/runner.go` 的 `StreamSink` 把一轮对话内部的四类事件回调出来，`internal/agent/loop.go` 的 `newLoopSink` 再分类推送到 channel / session。它们正是评审侧 `TraceEvent` 的来源。

```text
 运行层事件（StreamSink 回调）                    评审侧 TraceEvent / 节点映射
 ─────────────────────────────                  ─────────────────────────────
  OnReasoningDelta  思考过程增量      ────────▶  kind = decision（意图/分支判断依据）
  OnToolCall        请求调用工具      ────────▶  kind = tool，tool = call.Name
  OnToolResult      工具执行结果      ────────▶  tool 节点结果（用于校验）
  OnContentDelta    面向用户正文      ────────▶  kind = output（拼成 output_text）
```

```text
 真实 AG-UI/SSE 事件流（internal/channels/web.go 输出）   →   前端/评审抽取
 ──────────────────────────────────────────────────────     ─────────────
 data: {"type":"TOOL_CALL_START","toolCallName":"GetNovelDetailByID"}
 data: {"type":"TOOL_CALL_ARGS","delta":"{\"fid\":\"...\"}"}   →   tool 节点
 data: {"type":"TOOL_CALL_RESULT","content":"{...}"}          →   tool 结果（校验）
 data: {"type":"TEXT_MESSAGE_CONTENT","delta":"以下..."}       →   output 正文
```

> 设计文档进一步建议给 SSE 事件补 `node_id` / `skill` 字段，把「靠文本猜」升级为「结构化埋点」。

### 2.2 Path 必须表达「分叉」，不能拍平

Skill 常常「按条件走不同分支、不同终点有不同预期」。若把互斥分支拍平成一条 `required=false` 的线性列表，评审就无法区分「该走 A 却走了 B」。

`internal/skillreview/review.go` 的 `NodeDefinition` 用两个字段表达树形结构：**线性**用 `Next` 指向唯一后继；**分叉**用 `kind=decision` 节点的 `Branches` 列互斥分支（每个 `Branch` 带 `When` / `To` / `Expect`）。两者均可省略，旧线性 Path 完全兼容。

以真实的 `skills/kbcli/PATH.json` 为例，一个 Skill 可以包含**三层嵌套 decision**（线性部分用 `next`，分叉部分用 `branches`）：

```text
                    [ match_intent ]                          kind=input
                    "激活 Skill 并解析用户意图"
                          │ next
                          ▼
                 [ read_references ]                         kind=validation
                 "必须先读 references/kb-recall.md 和 sage.md"
                 "禁止跳过文档凭记忆拼接命令"
                          │ next
                          ▼
                 [ decision_main ]                           kind=decision
                 "查数场景 vs 专家问答（两条线互斥）"
                    ┌──────────────┴──────────────┐
          when=查数(字段/指标/数据)        when=专家问答(@expert:market)
          expect:{type:数据结果}            expect:{type:专家回答}
                    │                              │
                    ▼                              ▼
             [ node_recall ]                   [ node_sage ]    kind=tool
             tool=kbcli kb-recall              tool=sage
             "--text 必填,--scope 可选"         "--agent-id + --text"
                    │                              │
                    ▼                              ▼
         [ decision_search_mode ]            [ output_sage ]    kind=output
         "SQL 模式 vs DSL 模式"
            ┌──────────────┴──────────────┐
  when=电视剧播放指标        when=项目ID/剧集分类
  (MySQL 取数)              (ES 定位)
            │                              │
            ▼                              ▼
     [ node_sql_mode ]                [ node_dsl_mode ]        kind=tool
     tool=kbcli kb-search             tool=kbcli kb-search
     "--database + --sql 同传"         "--query SELECT ..."
            │                              │
            ▼                              ▼
      [ output_sql ]                [ check_capability ]       kind=decision
      kind=output                   "DSL 能否表达需求？"
                                        ┌──────────────┴──────────────┐
                              when=需 range/rank/exact/多库  when=简单查询(项目ID)
                              (DSL 表达不了 → MCP 兜底)     (只需 ID)
                                        │                              │
                                        ▼                              ▼
                              [ node_mcp_fallback ]          [ output_dsl ]   kind=output
                              tool=kb_search (MCP)
                                        │
                                        ▼
                                  [ output_dsl ]              kind=output
```

> 上图中每个 `decision` 节点的 `branches[*].when` 对应「进入该分支的条件」，`to` 指向后继节点，`expect`（`type` / `contains`）是该分支终点对应的预期结果——这正是「不同分叉有不同预期」在数据模型里的落点。Path 顶层 `exit` 字段为 `output_sql`（主出口）。

### 2.3 Path 从哪里来：双引擎抽取

Path 不是凭空手写，而是从 `SKILL.md` 正文推导。难点在于**真实 Skill 的「工具」形态五花八门**（MCP 的 `mcporter call`、CLI 的 `kbcli kb-search`、`bash` 脚本），正则启发式抓不全。`skills/kbcli/PATH.json` 正是这套抽取的产物——它同时覆盖了 CLI 工具（`kbcli kb-recall`、`kbcli kb-search`、`sage`）与 MCP 工具（`kb_search`）两类形态，完整剖析见 3.9。

`cmd/skill-review/llm_path.go` + `skills_api.go` 采用**双引擎**：

| 引擎 | 实现 | 特点 |
|---|---|---|
| LLM 抽取 | `llmExtractor.extract`（复用 `providers.Provider.Chat`） | 读语义，直接产出带分叉的 `PathDefinition`，可抓 MCP/CLI/bash 三类工具 |
| 规则兜底 | `derivePath`（正则启发式） | 零依赖、必定产出，识别触发词 / `references/` / `bash` 脚本 / 重要规则 / 异常处理 |

```text
        POST /api/skill/paths { name, content? }
                        │
                        ▼
               engine 参数取值？
          ┌────────────┼────────────────┐
     engine=rule     engine=auto     engine=llm
          │              │               │
          │              ▼               │
          │         llm 可用？            │
          │        ┌────┴────┐           │
          │       是        否          │
          │        │         │           │
          │        ▼         │           ▼
          │   LLM 抽取        │      LLM 抽取（强制）
          │   extract()       │       失败 → 502 报错
          │   成功？          │
          │   ├──否──┐       │
          │   ▼      │       │
          │ 回退规则版  │       │
          │   │      │       │
          ▼   ▼      ▼       ▼
        ┌─────────────────────────┐
        │  derivePath(name, md)    │  规则版兜底
        │  match_intent → ref →    │
        │  tool → verify → fallback│
        │  → produce_output        │
        └────────────┬────────────┘
                     ▼
              content 是否为草稿？
             ┌───────┴───────┐
           草稿              正式
             │                │
             ▼                ▼
         仅预览返回      写入 skills/<name>/PATH.json 缓存
```

> 生成有成本（尤其 LLM），因此结果落盘为 `PATH.json` 缓存，Web 加载时直接读缓存，只有主动「生成 / 重新生成」才重算。

### 2.4 意图匹配本身即入口节点

由于没有统筹 Skill，Path 最前面的 `match_intent` 节点代表的是**模型的意图匹配**，而非某个 Skill 的执行。保留它是因为「输入有没有命中正确的 Skill」是评审第一优先级要验证的（对应 `skill_not_selected` 失败码）。这要求评审体系把「Skill 选择」和「Skill 内部链路」作为两个**可分别定位**的维度，而非混为一体。

```text
         ┌───────────────────────────────────────────────┐
         │              评审的两个独立维度                  │
         ├───────────────────────┬───────────────────────┤
         │ ① Skill 选择是否正确    │ ② Skill 内部链路是否正确 │
         │    match_intent        │    Path/Node/Tool 链路  │
         │    → skill_not_selected│    → path_mismatch 等   │
         │    → forbidden_skill   │    → node_missing 等    │
         └───────────────────────┴───────────────────────┘
```

### 2.5 注意事项的「可断言化」

注意事项不能只留在自然语言备注里。对影响正确性的内容，必须转化为 `condition`、`forbidden_nodes`、输出断言或失败分支：

```text
 自然语言注意事项                      →  可断言规则（Expected 字段）
 ──────────────────────                  ─────────────────────────────
 "不得把模拟结果描述成真实联网结果"      →  OutputSpec.NotContain
 "禁止翻老 schema"                      →  ForbiddenNodes
 "禁止调用错误 Tool"                    →  tool_missing / forbidden 校验
 "必须先读 references"                  →  Node(kind=validation, required)
```

在 `Evaluate` 中对应 `notice_missed`、`output_content_forbidden`、`forbidden_node` 等失败码。

### 2.6 工程约束

- **零第三方依赖**：`internal/skills/frontmatter.go` 用受限的「`key: value` 单行解析器」而非引入 YAML 库，仅支持 Skill frontmatter 的真实形态。
- **Workspace 沙盒**：`read_file` 校验绝对路径 / 穿越 / symlink 逃逸（`internal/tools/read_file.go`）；`skills_api.go` 的 `safeName` 清洗 name 杜绝路径穿越。
- **KV Cache 友好**：L1 摘要与 always 正文进程启动时构建一次、全程不变；动态内容只追加在 context 末尾（`withStatusLine` 用 user 槽位挂状态、不改 system）。

---

## 三、整体架构

### 3.1 分层总览

```text
┌───────────────────────────────────────────────────────────────────────────────┐
│ ① 执行层（运行时，产生可观测事件）           internal/agent                       │
│                                                                               │
│   Loop.handle ──▶ Runner.RunCollect ──▶ Provider.Chat ──▶ Tools.Execute        │
│    (协调上下文)     (多轮对话循环)           (LLM)            (read_file/bash/…)  │
│         │                 │                                    │               │
│         │            StreamSink ◀── reasoning / tool_call / tool_result / content
│         ▼                 │                                    │               │
│   Bus.PublishOutbound ────┘（SSE / AG-UI 事件流，原始 trace）                    │
└────────────────────────────────────────┬──────────────────────────────────────┘
                                         │
                                         ▼
┌───────────────────────────────────────────────────────────────────────────────┐
│ ② Skill 层（被评审对象）                   internal/skills                      │
│   Loader（发现 + 三层披露） · frontmatter（元数据解析）                          │
│   skills/*/SKILL.md + references/  ──(抽取)──▶  PathDefinition                 │
└────────────────────────────────────────┬──────────────────────────────────────┘
                                         │
                                         ▼
┌───────────────────────────────────────────────────────────────────────────────┐
│ ③ 评审核心                                internal/skillreview                 │
│   Case(预期) ─┐                                                            │
│               ├──▶ Evaluate() ──▶ Failure[] + Metrics ──▶ Report             │
│   Run(实际)  ─┘                                                            │
│   modules.go：路径发现(active) → 测试数据集 → 单元测试 → 定向修复 → 全量回归   │
└────────────────────────────────────────┬──────────────────────────────────────┘
                                         │
                                         ▼
┌───────────────────────────────────────────────────────────────────────────────┐
│ ④ 命令 / 服务层                             cmd/skill-review                   │
│   main.go(CLI) · server.go(仪表盘) · skills_api.go(Skill 管理 + Path 生成)     │
│   llm_path.go(LLM 抽取) · web/index.html(三栏目 UI)                            │
└───────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 运行时执行时序（一次 Skill 调用的完整链路）

```text
 用户      Channel        Loop           Runner           LLM         Tool(read_file/bash)
  │           │             │               │               │               │
  │  发送消息  │             │               │               │               │
  ├──────────▶│  入站        │               │               │               │
  │           ├────────────▶│ handle()      │               │               │
  │           │             │ 组装 system   │               │               │
  │           │             │  + 历史 + user │               │               │
  │           │             ├──────────────▶│ RunCollect    │               │
  │           │             │               ├──────────────▶│ Chat(tools 定义)│
  │           │             │               │ ◀──reasoning delta──────────── │
  │           │             │               │ 模型请求 tool_call             │
  │           │             │               ├───────────────┼──▶ read_file(SKILL.md)
  │           │             │               │ ◀──tool_result──────────────── │
  │           │             │               ├──────────────▶│ Chat(带 tool 结果)
  │           │             │               │ ◀──content delta────────────── │
  │           │             │ ◀─────────────┤ 返回 Answer + Messages          │
  │           │ ◀───────────┤ 出站分片       │               │               │
  │           │             │ (SSE/AG-UI)   │               │               │
  │           │             │ 回写 session  │               │               │
  │ ◀─────────┤             │               │               │               │
```

### 3.3 核心数据模型关系（ER 图）

```text
 预期侧（Expected）                                     运行侧（Run / Trace）
 ───────────────────                                    ─────────────────────

 Case ─1:1─ Expected                        Run ─1:N─ TraceEvent
  │             │                                   (NodeID,Kind,Tool,
  │             ├─1:N─ SkillExpectation              Condition,Decision,Notes)
  │             │        (命中 Skill, required)
  │             ├─1:N─ ChainStep                    Run ─1:N─ ChainStep(实际链路)
  │             │        (跨 Skill 链路)             Run ─1:N─ ToolCalls
  │             ├─1:N─ NodeDefinition               Run ─1:N─ SelectedSkills
  │             │        └─0:N─ Branch              Run ─1:N─ AcknowledgedNotices
  │             │                (When/To/Expect)
  │             ├─1:N─ Notices(注意事项)
  │             └─1:1─ OutputExpectation(输出断言)

 PathDefinition ─1:N─ NodeDefinition
                        kind ∈ {input, decision, validation, tool, output, fallback}

 判定侧（Evaluate 产物）
 ─────────────────────
 Evaluate(Expected, Run) ──▶ CaseResult{ Passed, Failures[] }
                                     └─ Failure{ Code, Message, FailedNode,
                                                 ExpectedPath, ActualPath }
 Report ─1:1─ Metrics(8 指标) + 1:N─ CaseResult
```

### 3.4 评审引擎：`Evaluate` 断言流程与失败码

```text
 Evaluate(cases, runs, skillVersion)
        │
        ▼
 按 CaseID 匹配 Run（缺 → missing_run）
        │
        ▼
 ① Skill 命中  ── 未命中必选 Skill ──▶ skill_not_selected
        │         命中禁止 Skill  ──▶ forbidden_skill
        ▼
 ② 路径        ── PathID 不在实际路径 ──▶ path_mismatch
        ▼
 ③ 链路        ── 缺步骤 ──▶ chain_step_missing
        │         缺工具 ──▶ tool_missing
        ▼
 ④ 注意事项    ── 未确认 ──▶ notice_missed
        ▼
 ⑤ 节点        ── 缺节点 ──▶ node_missing
        │         触发禁止节点 ──▶ forbidden_node
        │         节点缺工具 ──▶ tool_missing
        ▼
 ⑥ 输出        ── 类型不符 ──▶ output_mismatch / output_type_mismatch
                  缺内容 ──▶ output_content_missing
                  含禁止 ──▶ output_content_forbidden
        ▼
 CaseResult.Passed = (len(Failures) == 0)
        ▼
 聚合 8 项 Metrics → Report（JSON / Markdown）
```

**8 项指标**（拆维度、不合并成单一总分，避免用总分掩盖诊断信息）：

| 指标 | 计算方式 |
|---|---|
| Skill 命中率 | 命中预期 Skill 数 / 必选 Skill 数 |
| Path 覆盖率 | 已执行 Path 数 / Path 总数 |
| 节点覆盖率 | 已执行 Node 数 / Node 总数 |
| 路径匹配率 | 路径符合预期的用例数 / 用例总数 |
| 节点通过率 | 通过节点数 / 被断言节点数 |
| 链路通过率 | 通过链路步骤数 / 被断言步骤数 |
| 注意事项通过率 | 已确认注意事项数 / 必须遵守数 |
| 输出通过率 | 满足输出断言的用例数 / 用例总数 |

### 3.5 报告输出（`report.go`）

- `WriteJSON`：机器可读，供后续程序比较、查询、自动化处理；
- `WriteMarkdown`：开发者可读，含总体结果、指标表、用例结果表、失败详情（失败码 + 失败节点）。

`Report.SortResults()` 保证用例顺序稳定，便于跨版本比较（回归）。

### 3.6 命令与本地仪表盘（`cmd/skill-review`）

- **CLI**（`main.go`）：`-cases` / `-runs` / `-paths` / `-skill-version` 输入，`-markdown` / `-json` 输出，`-serve` 启动仪表盘。
- **仪表盘**（`server.go`）：单页 `/` + `/api/data`（modules + report + paths），只读、不直接改 Skill。
- **Skill 管理 API**（`skills_api.go`）：`GET /api/skills`、`GET|PUT /api/skill?name=`、`GET|POST /api/skill/paths`。
- **LLM 抽取器**（`llm_path.go`）：按 `SZABOT_PROVIDER` + API key 环境变量构造 Provider，抽取带分叉的 `PathDefinition`，并对模型输出的 ```json 围栏 / 前后缀做容错解析。

```text
┌────────────────┬───────────────────────────────────────────────────────────┐
│  Skill Review   │  ① Skill 管理      ② 测试用例       ③ 评估对比             │
│                │                                                           │
│ ● Skill 管理    │  Skill 列表 │ SKILL.md 编辑器 │ 预设 Path（分叉树）          │
│ ○ 测试用例      │  (name/desc)│ (frontmatter+正文) │ (n8n 风格横向画布)        │
│ ○ 评估对比      │                                                           │
│ ○ 回归(规划)    │  评估对比视图：                                             │
│                │  ┌ 指标区 ───────────────────────────────────────────┐    │
│ ── 上下文 ──    │  │ 命中率 | 覆盖率 | 节点通过率 | 链路通过率 | 输出通过率 │    │
│ 当前 Skill: …   │  └──────────────────────────────────────────────────┘    │
│ 版本: …         │  预期路径: [detect]→[load]→[run]→[verify]→[output]        │
│                │  实际路径: [detect]→[load]→[run]→[✗缺verify]→[output]       │
│                │                                   ▲ 失败节点高亮 + 失败码    │
└────────────────┴───────────────────────────────────────────────────────────┘
```

### 3.7 测试用例构建闭环（录制 → 抽取 → Case/Run → 回归）

```text
 ① 实时录制 / 粘贴            ② 解析抽取                  ③ 人工确认             ④ 评估 / 回归
 ────────────────            ─────────────              ──────────            ──────────────
 用户输入                       SSE 事件流                一次「好」运行
    │   POST /api/send            │ 按 kind 分类/编号          │
    ▼                            ▼                          ├──▶ 保存为 Run（实际）
 Agent 执行               selected_skills                   │
    │   SSE 事件流            actual_path                    └──▶ 固化为 Case.Expected（基线）
    ▼                        tool_calls
 原始 trace                 execution_chain                未来 Skill 改动后重跑：
                            output_text                    new Run vs Expected
                                                           ──▶ 发现回归 / 新修复 / 保持
```

> 核心思路：**一次好的运行，既是一条 `Run`，也可以固化成一条 `Case.Expected` 作为基线**。以后 Skill 改动后重新运行，新 `Run` 再跟这条 `Expected` 对比，就能发现回归。

### 3.8 流程阶段（`modules.go`）

评审系统按 5 个模块组织，当前只有「路径发现」处于 `active`，其余保留稳定模块边界待实现：

| 模块 | 状态 |
|---|---|
| 路径发现（生成并校验 Skill Path） | active |
| 测试数据集（为用户输入准备评审合同） | planned |
| 单元测试（执行链路与覆盖率） | planned |
| 定向修复（关联失败节点与 Skill 修改） | planned |
| 全量回归（验证修改没有回归） | planned |

### 3.9 实例剖析：`skills/kbcli/PATH.json`

上面各节的概念，落到一个真实 Path 上（`skills/kbcli/PATH.json`，对应 `skills/kbcli/SKILL.md`）。

#### （1）叶子路径枚举 → 生成用例

kbcli 的 Path 含 3 个 decision 节点（`decision_main` / `decision_search_mode` / `check_capability`），从 `match_intent` 出发到各 output 叶子，共枚举出 **4 条互斥路径**（对应前端 `enumerateCasePaths`）：

```text
 ① 查数 → SQL 模式：
    match_intent → read_references → decision_main(查数) → node_recall
      → decision_search_mode(SQL) → node_sql_mode → output_sql

 ② 查数 → DSL 模式 → MCP 兜底：
    match_intent → read_references → decision_main(查数) → node_recall
      → decision_search_mode(DSL) → node_dsl_mode → check_capability(MCP 兜底)
      → node_mcp_fallback → output_dsl

 ③ 查数 → DSL 模式 → 简单查询：
    match_intent → read_references → decision_main(查数) → node_recall
      → decision_search_mode(DSL) → node_dsl_mode → check_capability(简单查询)
      → output_dsl

 ④ 专家问答：
    match_intent → read_references → decision_main(专家问答)
      → node_sage → output_sage
```

#### （2）分支 expect → Case.Expected 映射

以「① 查数 → SQL 模式」为例，`PATH.json` 里 `decision_search_mode` 的 SQL 分支 `expect` 是 `{"type":"指标数据","contains":["播放量","收视率"]}`，枚举成用例时转成 `Expected`：

```json
{
  "case_id": "case-kbcli-query-sql",
  "input": { "user_message": "查《逐玉》的播放量" },
  "expected": {
    "path_id": "path_kbcli",
    "expected_skills": [ { "name": "kbcli", "required": true } ],
    "nodes": [
      "match_intent", "read_references", "decision_main",
      "node_recall", "decision_search_mode", "node_sql_mode", "output_sql"
    ],
    "execution_chain": [
      { "id": "node_recall",   "kind": "tool", "tool": "kbcli kb-recall", "required": true },
      { "id": "node_sql_mode", "kind": "tool", "tool": "kbcli kb-search", "required": true }
    ],
    "output_spec": { "type": "指标数据", "contains": ["播放量", "收视率"] }
  }
}
```

> 注意 `expected_skills` 里命中的是 `kbcli`，而 `execution_chain` 里的 `tool` 是 `kbcli kb-recall` / `kbcli kb-search`——「Skill 选择」与「Skill 内部工具链路」是两层独立的断言维度（对应 2.4）。

#### （3）一次 Evaluate 对比（预期 vs 实际）

假设实际运行中，模型对「查播放量」错误地走了 DSL 模式（而非预期的 SQL 模式），`Evaluate` 如何定位：

```text
 预期路径:  ... → decision_search_mode → [node_sql_mode] → output_sql
 实际路径:  ... → decision_search_mode → [node_dsl_mode] → check_capability → output_dsl
                                              ▲
                                        失败节点高亮
```

对应产生的失败码：

```text
 Failure{ code: "node_missing", failed_node: "node_sql_mode",
          message: "实际路径缺少预期节点" }
 Failure{ code: "output_type_mismatch",
          message: "最终输出类型不符合预期" }        ← 实际产出「元数据」而非「指标数据」
```

由此可定位到 `decision_search_mode` 的 SQL/DSL 分支判断出错——这正是「分叉表达」让评审能从「总分」下沉到「哪个分支出错」的价值所在。

---

## 附：关键文件索引

| 文件 | 职责 |
|---|---|
| `internal/skills/skill.go` | Skill / Meta / Requires 定义与三层披露设计 |
| `internal/skills/loader.go` | 技能发现、同名覆盖、L1 摘要与 always 正文生成 |
| `internal/skills/frontmatter.go` | 受限的 frontmatter 解析（零 YAML 依赖） |
| `internal/agent/runner.go` | `RunCollect` 对话循环 + `StreamSink` 可观测事件 |
| `internal/agent/loop.go` | 上下文组装、session 持久化、事件分类推送 |
| `internal/skillreview/review.go` | 数据模型 + `Evaluate` 断言引擎 + 指标聚合 |
| `internal/skillreview/report.go` | JSON / Markdown 报告输出 |
| `internal/skillreview/modules.go` | 5 阶段流程定义 |
| `cmd/skill-review/main.go` | CLI 入口 |
| `cmd/skill-review/server.go` | 本地只读仪表盘 |
| `cmd/skill-review/skills_api.go` | Skill 管理 API + 规则版 Path 生成 |
| `cmd/skill-review/llm_path.go` | LLM Path 抽取器（双引擎之一） |
| `cmd/skill-review/web/index.html` | 三栏目 UI（含分叉树渲染 `renderTree`、用例枚举 `enumerateCasePaths`） |
