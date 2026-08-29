# Context 压缩与 Trace 改造报告

## 1. 背景

在 2026-08-24 的两个 Run 中，模型调用 `web_search` 后因 Context 超过 6000 token 上限而结束。Run 和 Trace 已经落盘，但 Run 状态为 `budget_exceeded`，因此用户消息和最终回答没有追加到 Conversation。

旧版本的 Context 结果主要依赖通用截断，搜索结果中的标题、来源和摘要可能被一起截断，且 Trace 中的上下文决策存在重复事件。

## 2. 本次改动

### 2.1 合并 Context 事件

移除独立的 `context.decision` Trace event，将决策字段合并到 `context.strategy.applied`。

现在一条策略事件同时记录：

- `layer`
- `action`
- `trigger`
- `attempted_layers`
- `reason`
- `source_id` / `artifact_id`
- `original_bytes` / `context_bytes`
- `tokens_before` / `tokens_after`
- `reversible`

这样可以避免同一决策写入两条重复事件，同时保留完整排障信息。

### 2.2 Web 工具专属预览压缩

`internal/agent/tool_context.go` 现在根据工具名选择预览策略：

- `web_search`：优先保留搜索摘要、结果标题、URL 和简短内容；完整结果仍写入 Artifact。
- `web_fetch`：优先保留页面标题和来源 URL，只压缩正文内容。
- 其他工具：继续使用通用的头尾预览策略。

当 Artifact 存储不可用时，仍会使用受限 preview 作为兜底，并记录相应的 Context 策略。

### 2.3 Trace 按模型步骤分组

Web Trace 页面按照 `model_step` 对事件分组：

```text
Run lifecycle
Model step 1
Model step 2
```

同一模型步骤中的模型请求、工具执行、Context 处理和 Artifact 创建会显示在同一组内；原始 Payload 和详情面板仍然保留。

## 3. Trace 对比

### 旧 Trace

文件：`sessionlogs/traces/b1fe750ac0233fa000be6889782495ca27e8cdf94f8273800408f8ca10899986.jsonl`

```text
第一次 web_search 后：6054 tokens
第二次 web_search 后：8453 tokens
最终状态：budget_exceeded
```

### 新 Trace

文件：`sessionlogs/traces/e26b3d05244560a70687da922297f2f9ba4a2031c482c97ecee704863a030b8f.jsonl`

```text
第一次 web_search：11143 bytes，Artifact preview，5976 tokens
第二次 web_search：6745 bytes，Artifact preview，6117 tokens
最终状态：budget_exceeded
```

新版本将最终 Context 从 8537 token 降低到 6187 token，说明工具专属 Artifact 预览已经生效。但第二次搜索加入时，第一次结果已经使 Context 接近上限，最终仍超过 6000 token。

需要注意：这两份 Trace 实际执行的工具都是 `web_search`，没有真正执行 `web_fetch`；`web_fetch` 只出现在工具定义中。因此 `web_fetch` 的专属策略还需要后续真实 Run 验证。

## 4. 当前边界

目前压缩策略已经可以把大工具结果外置，但还没有在加入第二个大结果前主动释放已消费的旧工具结果。下一步可以实现：

1. 在达到 80% 预警区时提前生成更短 preview；
2. 对已经消费且低价值的工具结果执行 WorkingContext 删除；
3. 对搜索结果做去重和相关性筛选；
4. 真实执行 `web_fetch`，验证标题、来源和正文压缩效果；
5. 在最终超限前保留最小可用 Context，避免整个 Run 直接失败。

## 5. 验证

已通过：

```text
go test ./...
```

提交信息：

```text
04dc480 Improve context compression and trace grouping
```

该提交已推送到 `https://github.com/sza0415/yomi` 的 `master` 分支。
