# Skill 评审系统实现计划

## 1. 背景与目标

当前 Skill 评审容易停留在“给出一个总分”。这种方式无法回答以下问题：

- 哪条执行路径被触发；
- 哪个分支或节点执行错误；
- 实际结果与预期结果差异在哪里；
- 修改 Skill 后是否修复问题；
- 修改是否影响了其他已经通过的场景。

本项目针对多 Skill Agent 建立可复现的系统级评审流程，将一次用户输入转化为可检查的评审合同：

```text
解析 Skill 集合 → 构建用户输入测试集 → 检查命中 Skill 与完整链路 → 检查注意事项与最终输出 → 定向修改 → 全量回归
```

## 2. 项目范围

### 2.1 本期范围

- 针对多 Skill Agent 建立系统级测试用例模型；
- 为每个 Skill 生成可组合的 Path 和 Node；
- 检查用户输入命中的 Skill 集合及其优先级；
- 检查跨 Skill 的完整执行链路和 Tool 调用；
- 检查 Skill 中的注意事项是否被遵守；
- 检查最终输出的类型、关键内容和禁止内容；
- 记录 Agent 实际执行路径与工具调用；
- 输出分支覆盖率、节点通过率和失败定位结果；
- 支持修改后的全量回归；
- 保留测试结果，便于比较不同版本的 Skill。

### 2.2 暂不纳入

- 复杂的自动修复和自动提交；
- 线上流量的自动采样；
- 跨模型的统一评分基准。

### 2.3 当前实现边界

当前阶段只实现评审系统的模块边界、Path / Node / System Test Contract / Trace / Evaluator / Report 等代码能力，不内置测试数据集。真实用户输入、期望命中 Skill、预期链路和回归样本将在后续作为独立数据工程接入。

## 3. 核心概念

### 3.1 Path

一条完整的 Skill 执行路径，表示从输入条件到最终出口的完整链路。

```text
入口条件 → 运行节点 → 判断分支 → 分支出口
```

Path 需要从 Skill 的正文、工具说明、条件判断和异常处理规则中推导出来，不能只把几条测试输入直接命名成 Path。每条 Path 应回答：

- 什么输入条件会进入这条路径；
- 哪些前置条件必须先满足；
- 哪些判断会决定分支；
- 调用了哪些 Tool，调用顺序和参数有什么要求；
- 哪些结果校验或注意事项必须执行；
- 工具失败、参数缺失和不适用时从哪里退出。

### 3.2 Node

Path 中可被观察、断言或定位的执行单元。除了 Skill 加载和结果生成，还应扩展记录以下节点：

| 节点类型 | 典型内容 | 是否通常需要断言 |
|---|---|---|
| `input` | 识别 `vid`、`cmsid`、`feedid` 或平台 | 是 |
| `validation` | 参数非空、格式合法、权限满足 | 是 |
| `decision` | 条件判断、优先级判断、分支选择 | 是 |
| `tool` | 调用搜索、读取、解析、执行等工具 | 是，包含工具名和关键参数 |
| `output` | 结果格式、必填字段、来源信息 | 是 |
| `fallback` | 失败处理、重试、降级、人工确认 | 按场景断言 |
| `note` | 防止误判或伪造结果的注意事项 | 应转化为约束或人工复核项 |

工具调用和注意事项都可以作为节点。注意事项不能只留在自然语言备注里；对于会影响正确性的内容，应转化为 `condition`、`forbidden_nodes`、输出断言或失败分支。

### 3.3 System Test Case

一条以用户输入为核心的系统级测试记录。它不只描述“应该走哪条 Path”，还要描述整个 Agent 的预期行为：

```text
用户输入
  → 期望命中的 Skill 集合
  → 跨 Skill 执行链路与 Tool 调用
  → Skill 注意事项 / 禁止行为
  → 最终输出断言
```

对应的 JSON 结构包括：

```json
{
  "case_id": "system-case-001",
  "input": {"user_message": "请分析并核对来源"},
  "expected": {
    "expected_skills": [
      {"name": "content-expert", "required": true},
      {"name": "source-checker", "required": true}
    ],
    "execution_chain": [
      {"id": "call-content-sim", "skill": "content-expert", "kind": "tool", "tool": "content-sim", "required": true}
    ],
    "notices": ["不得把模拟结果描述成真实联网结果"],
    "output_spec": {
      "type": "analysis_report",
      "contains": ["结论"],
      "not_contains": ["已联网核实"]
    }
  }
}
```

### 3.4 Run

一次测试执行实例，除了实际节点路径，还要记录 `selected_skills`、`execution_chain`、`tool_calls`、`acknowledged_notices` 和最终输出。

## 4. 总体架构

```text
测试用例文件
      ↓
Test Case Loader
      ↓
Skill Runner / Agent Adapter
      ↓
Execution Trace Collector
      ↓
Path Matcher + Assertion Engine
      ↓
Metrics Aggregator
      ↓
Markdown / JSON 评审报告
```

### 4.1 模块职责

| 模块 | 职责 |
|---|---|
| Case Loader | 读取和校验测试数据集 |
| Skill Runner | 使用指定 Skill 和 Agent 配置执行用例 |
| Trace Collector | 记录节点、分支、工具调用和输出 |
| Path Matcher | 比较预期路径与实际路径 |
| Assertion Engine | 校验节点、结果和约束 |
| Metrics Aggregator | 计算覆盖率和通过率 |
| Report Generator | 生成机器可读和人类可读报告 |

## 5. 实施阶段

### 阶段一：梳理 Skill 分支

1. 读取 Skill 正文，提取输入、Tool、判断、输出和异常处理规则。
2. 先画出分支图，再为每条从入口到出口的链路定义唯一 Path ID。
3. 将每条 Path 拆成有序 Node，并标记 `kind`、`required`、`tool` 和 `condition`。
4. 把“不得调用错误 Tool”“失败时不得伪造结果”等注意事项转成可断言规则。
5. 标记必经节点、可选节点、禁止节点和异常出口。
6. 建立 Path 清单，作为测试覆盖基线。

初始场景至少覆盖：

- `vid` 输入；
- `cmsid` 输入；
- `feedid` 输入；
- 微视频平台；
- 新闻平台；
- 多种条件同时存在；
- 缺少必要参数；
- 外部数据获取失败。

### 阶段二：定义测试数据格式

当前最小实现使用 JSON 保存测试用例和 Path 定义，避免引入新的 YAML 解析依赖。格式稳定后可以再提供 YAML 适配层。

```yaml
case_id: skill-review-001
name: vid 输入场景
skill_version: current
input:
  user_message: "请处理这个视频"
  params:
    vid: "real-video-id"
expected:
  path_id: path_video
  nodes:
    - detect_input
    - load_skill
    - process_video
  output:
    type: video_result
  constraints:
    - 不得进入新闻处理分支
    - 必须使用视频处理链路
tags:
  - happy-path
  - vid
```

数据集应同时包含：

- 正常用例；
- 边界用例；
- 参数冲突用例；
- 无效输入用例；
- 工具失败用例；
- 历史线上失败用例；
- 不适用场景用例。

### 阶段三：接入执行与路径追踪

为 Agent 执行层增加统一的 Trace 记录。每次执行至少记录：

```json
{
  "run_id": "run-001",
  "case_id": "skill-review-001",
  "skill_version": "git-sha-or-version",
  "status": "failed",
  "expected_path": ["detect_input", "load_skill", "process_video"],
  "actual_path": ["detect_input", "load_skill", "process_article"],
  "tool_calls": [],
  "failed_node": "process_article",
  "failure_reason": "vid 被错误识别为新闻内容",
  "output": "..."
}
```

如果现有执行框架无法直接暴露内部节点，应先通过以下方式补充可观测性：

- 在 Skill 关键分支输出结构化事件；
- 为工具调用增加统一事件包装；
- 为每个 Path 和 Node 分配稳定 ID；
- 将原始输出和结构化 Trace 分开保存。

### 阶段四：实现断言与评分

评分应拆成多个维度，不使用单一总分替代诊断信息。

建议指标：

| 指标 | 计算方式 |
|---|---|
| Skill 命中率 | 命中预期 Skill 数 / 必须命中的 Skill 数 |
| Path 覆盖率 | 已执行 Path 数 / Path 总数 |
| 节点覆盖率 | 已执行 Node 数 / Node 总数 |
| 路径匹配率 | 实际路径符合预期的用例数 / 用例总数 |
| 节点通过率 | 通过节点数 / 被断言节点数 |
| 工具调用准确率 | 实际调用符合预期的用例数 / 需要工具调用的用例数 |
| 输出合规率 | 满足输出断言的用例数 / 用例总数 |
| 链路通过率 | 通过的执行链路步骤数 / 被断言链路步骤数 |
| 注意事项通过率 | 已确认注意事项数 / 必须遵守的注意事项数 |
| 回归保持率 | 修改后仍通过的原通过用例数 / 原通过用例数 |

断言优先级建议为：

1. 必须命中正确的 Skill 集合；
2. 必须经过预期的跨 Skill 执行链路；
3. 必须经过必需 Node 并调用正确 Tool；
4. 不得触发禁止 Skill、禁止 Node 或违反注意事项；
5. 输出结构、关键内容和事实边界符合预期。

### 阶段五：生成评审报告

每次 Run 生成两种报告：

- JSON：供后续程序比较、查询和自动化处理；
- Markdown：供开发者阅读和定位问题。

Markdown 报告至少包含：

```text
基本信息
总体结果
覆盖率指标
失败用例列表
预期路径与实际路径对比
失败节点和原因
建议修改位置
与上一次 Run 的差异
```

失败用例示例：

| Case | 预期 Path | 实际 Path | 失败节点 | 原因 |
|---|---|---|---|---|
| skill-review-001 | `path_video` | `path_article` | `process_article` | 输入类型识别错误 |

### 阶段六：定向修复与全量回归

根据失败节点定位 Skill 修改范围：

| 失败类型 | 优先检查位置 |
|---|---|
| 输入识别错误 | 触发条件、参数解析和优先级 |
| 分支选择错误 | 判断顺序、冲突规则和默认分支 |
| 节点遗漏 | 执行步骤和工具调用约束 |
| 输出不符合预期 | 输出格式、字段和结束条件 |
| 工具失败未处理 | 异常分支、重试和兜底规则 |
| 平台适配错误 | 平台识别规则和专用处理路径 |

每次修改后必须执行全量测试集，而不是只重跑失败用例。需要重点检查：

- 原失败用例是否修复；
- 原通过用例是否保持通过；
- Path 覆盖率是否下降；
- 是否出现新的高优先级失败；
- 输出格式是否发生非预期变化。

## 6. 目录建议

```text
skill-review/
├── cases/
│   ├── paths.json
│   ├── happy-path.json
│   ├── edge-cases.json
│   └── regression.json
├── runner/
│   ├── case_loader
│   ├── skill_runner
│   ├── trace_collector
│   └── assertions
├── reports/
│   ├── json/
│   └── markdown/
└── README.md
```

实际落地时应优先复用仓库已有的测试、日志和配置目录，避免引入平行的执行框架。

## 7. 里程碑与交付物

### M1：Path 建模完成

- 完成目标 Skill 的分支清单；
- 为每条 Path 和 Node 分配稳定 ID；
- 明确核心路径和异常路径。

### M2：最小可运行测试链路

- 支持加载一组 JSON 测试用例；
- 能执行 Agent；
- 能保存预期和实际 Trace；
- 能判断单条用例是否通过。

### M3：评审报告完成

- 输出覆盖率指标；
- 输出失败节点；
- 输出预期路径和实际路径差异；
- 支持 JSON 和 Markdown 两种格式。

### M4：回归能力完成

- 支持固定回归数据集；
- 支持修改前后结果比较；
- 支持检测新增失败；
- 形成发布前验收流程。

## 8. 验收标准

满足以下条件即可认为本期实现完成：

- 每个核心 Skill 组合场景至少有一条以用户输入为入口的可执行测试用例；
- 每条测试用例都能记录实际命中的 Skill 集合和完整执行链路；
- Skill 选择错误、链路错误和 Path 错误可以分别定位；
- 每条测试用例都能检查 Skill 注意事项是否被确认或遵守；
- 最终输出的类型、关键内容和禁止内容可以被断言；
- 评审报告同时提供通过结果和失败原因；
- 修改 Skill 后可以运行全量回归；
- 能识别新增失败和原有失败是否修复；
- 测试结果包含 Skill 版本和测试集版本，可重复生成；
- 不依赖人工只查看总分来判断是否通过。

## 9. 后续扩展

- 接入历史失败案例自动生成回归用例；
- 支持多个模型或 Agent 配置的对比评审；
- 支持按 Path 聚合失败趋势；
- 支持基于失败节点生成 Skill 修改建议；
- 接入 CI，在 Skill 变更后自动运行评审；
- 增加人工复核标记和评审结论。

## 10. 本地可视化界面

评审结果提供本地 Web 仪表盘，用于查看 Path 和失败定位：

```bash
go run ./cmd/skill-review \
  -cases <cases.json> \
  -paths <paths.json> \
  -runs <runs.json> \
  -skill-version local \
  -serve -addr :8090
```

当前阶段没有提交默认数据文件；上述输入由后续测试数据集模块提供。

打开 `http://localhost:8090` 后可以查看：

- Path 清单和入口条件；
- 节点顺序、节点类型和 Tool 名称；
- 用例通过情况；
- Path / Node 覆盖率；
- 失败节点、错误类型和注意事项。

界面只读取评审 JSON，不直接修改 Skill 或测试数据，作为评审和回归结果的只读查看器。
