# Sage 对话

``` bash
# sage问答模块
kbcli sage ask [options]
```

- **执行查询**：根据用户意图选择子 Agent，调用 `kbcli sage ask [options]`；
  - 多轮对话：工具输出中由 `<thread_id>` 和 `</thread_id>` 包裹的为当前对话的 `thread-id`, 需要在后续调用中通过 `--thread-id <thread_id>` 参数传入以维持上下文连续性。
  - 状态轮训：对子Agent的调用可能需要一定时间（大于300s），为减少上下文窗口的消耗，单次轮训的等待时间应控制在180s以上，且不超过300s。

---

## @expert:market

影视项目营销分析与方案撰写

- **触发词**：营销方案、营销分析、宣传方案、推广方案、营销策略、怎么营销、怎么宣传、弹幕分析、话题分析、收视分析。
- **输入参数**：对上下文关于 `营销专家` 的内容进行总结，并携带相关意图
- **结果处理** 按照以下要求进行处理：
  - 当你接收到工具返回的数据时，请定位到 `<text>` 和 `</text>` 标签之间的内容
  - 仔细阅读上述 `<text>` 标签内的纯文本内容。
  - 忽略所有格式干扰，提取出该文本的核心逻辑与关键信息。
  - 请整理并输出一份该内容的要点摘要，字数严格控制在 **200字左右**（180 - 220字之间），语言需精炼、直白、无废话。

```bash
kbcli sage ask --agent-id=market_expert_agent [--thread-id <thread_id>] --text <text>
```

---

## @expert:variety

综艺研发策划专家，为制片人提供综艺**模式分析与策划**服务，覆盖找模式、拆解模式、本土化改编、选题策划、赛制设计、风险评估等全链路诉求。
- **输入参数**：对上下文中关于 `综艺模式专家`的内容进行总结，并携带相关意图
- **结果处理** 按照以下要求进行处理：
  - 当你接收到工具返回的数据时，请定位到 `<text>` 和 `</text>` 标签之间的内容
  - 仔细阅读上述 `<text>` 标签内的纯文本内容。
  - 忽略所有格式干扰，提取出该文本的核心逻辑与关键信息。
  - 请整理并输出一份该内容的要点摘要，字数严格控制在 **200字左右**（180 - 220字之间），语言需精炼、直白、无废话。

```bash
kbcli sage ask --agent-id=variety_expert_agent [--thread-id <thread_id>] --text <text>
```

---

## @expert:variety-marketing

综艺项目营销复盘与日周报撰写

- **触发词**：综艺营销日报、综艺营销周报、综艺市场宣发、怎么写日报、怎么写周报、综艺舆情分析、综艺热搜分析。
- **输入参数**：对上下文关于 `综艺营销专家` 的内容进行总结，并携带相关意图。
- **结果处理** 按照以下要求进行处理：
  - 当你接收到工具返回的数据时，请定位到 `<text>` 和 `</text>` 标签之间的内容
  - 仔细阅读上述 `<text>` 标签内的纯文本内容。
  - 忽略所有格式干扰，提取出该文本的核心逻辑与关键信息。
  - 请整理并输出一份该内容的要点摘要，字数严格控制在 **200字左右**（180 - 220字之间），语言需精炼、直白、无废话。

```bash
kbcli sage ask --agent-id=variety_marketing_agent [--thread-id <thread_id>] --text <text>
```

---

## @expert:novel

- **输入参数**：按照下列要求拼接 `--text` 参数
  - **reference** 引用 `@script-id:{script_id}` 部分内容
  - **text** 对上下文中关于 `小说专家` 的内容进行总结
  - 按照模板 `{reference},{text}` 对 `--text` 进行拼接，如：`@script-id:{foo}，对这个文件进行分析`
  - 请整理并输出一份该内容的要点摘要，字数严格控制在 **200字左右**（180 - 220字之间），语言需精炼、直白、无废话。

```bash
kbcli sage ask --agent-id=novel_agent --thread-id [--thread-id <thread_id>] --text <text>
```


## @expert:script

剧本专家
- **输入参数**：拼接 `--text` 参数
  - **text** 对上下文中关于 `剧本专家` 的内容进行分析
  - 按照模板 `{reference},{text}` 对 `--text` 进行拼接，如：`proj_id:{proj_id},scriptAnalysisId:{scriptAnalysisId},scriptVersionId:{scriptVersionId}，对上下文中关于 剧本专家 的内容进行分析"`
- **解析规则（严格静默，仅模型内部使用）**：
  - 该解析过程**严禁以任何形式呈现给用户**，包括但不限于：
    - 不得输出 `proj_id:...`、`scriptAnalysisId:...`、`scriptVersionId:...` 等字段名或字段值；
    - 不得输出"根据输入解析"、"解析结果如下"、"我来帮你…首先需要创建会话…然后解析…"等思考/铺垫文本；
    - 不得用列表、表格、代码块、引用块等任何形式展示解析后的 ID。
- **结果处理** 按照以下要求进行处理：
  - 不得输出 `proj_id:...`、`scriptAnalysisId:...`、`scriptVersionId:...` 等字段名或字段值；
  - 当你接收到工具返回的数据时，请定位到 `<text>` 和 `</text>` 标签之间的内容
  - 仔细阅读上述 `<text>` 标签内的纯文本内容。
  - 忽略所有格式干扰，提取出该文本的核心逻辑与关键信息。
  - 请整理并输出一份该内容的要点摘要，字数严格控制在 **200字左右**（180 - 220字之间），语言需精炼、直白、无废话。

```bash
kbcli sage ask --agent-id=script_expert_agent --thread-id <thread_id> --text <text>
```
