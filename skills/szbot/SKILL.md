---
name: szbot
description: 影库命名空间路由入口：意图识别与路由（查数/专家问答 → kbcli）、输入消歧与公共护栏
always: true
---

# 影库命名空间路由入口（szbot SKILL）

> **本文件职责**：① 输入定性与路由（意图 → kbcli 子能力）② 高频歧义消解 ③ 公共护栏。
>
> ⛔ **不在此展开**：kbcli 的命令参数、双模式边界、专家路由表——**单一真源**是 `skills/kbcli/SKILL.md` 与其 `references/`，按需 `read_file`。
>
> **通用约定**：
> - **入口路径** = `skills/kbcli/SKILL.md`（参考文档在其 `references/` 下），用 read_file 读取。
> - **先读后调**：调用 kbcli 任何子命令前先读 `SKILL.md` + 对应 `references/`，禁止凭记忆/猜测构造参数。
> - 任何用户输入先当**查询请求**处理；不确定也先查，查无再说。
> - ⛔ **短词/短语（含像成语/祝福/日常用语的，如"静水流深""主角""来战"）默认是影视综实体（剧名/人名/公司名），必须先查（走查数链路），禁止当闲聊/问候直接回复**；不确定也先查，查无再说。
> - 📌 **短词分流（A 具名实体 / B 动作意图）**：短词先判类别——**A 具名实体**（能当作一个具体名字检索：剧名/人名/公司名/IP，如"静水流深""来战"）→ 直接路由查询，查无再说；**B 动作/泛意图**（动作或泛类目、缺具体实体宾语，如"找演员""查数据""看进度""分析一下"）→ 缺可检索实体，**先向用户澄清补全对象再查**，⛔ 不得把动作词当实体名硬查、也不得凭理解直接作答。**判据 = 能否作为一个具体实体名去检索**。

---

## §1 路由表（意图 → kbcli 子能力）

当前命名空间唯一执行技能是 `kbcli`，按下表把意图落到其子能力：

| 用户意图 | kbcli 子能力 | 前置阅读 |
|---|---|---|
| 影视综数据/字段/指标查询（**一切查数的第一步**） | `kb-recall` 两段式召回 → 按召回段分流 | `skills/kbcli/references/kb-recall.md` |
| 项目ID / 剧集分类定位（ES 元数据） | `kb-search --query`（DSL 模式） | 同上 |
| 电视剧播放指标取数（MySQL） | `kb-search --database <db> --sql <SQL>`（SQL 模式） | 同上 |
| 营销方案/营销分析/宣传推广/弹幕·话题·收视分析 | `sage ask --agent-id=market_expert_agent` | `skills/kbcli/references/sage.md` |
| 综艺模式分析/策划/本土化/赛制设计 | `sage ask --agent-id=variety_expert_agent` | 同上 |
| 综艺营销日报/周报/舆情/热搜分析 | `sage ask --agent-id=variety_marketing_agent` | 同上 |
| 小说专家分析 | `sage ask --agent-id=novel_agent` | 同上 |
| 剧本专家分析 | `sage ask --agent-id=script_expert_agent` | 同上 |

> ⛔ **两条线互斥**：查数场景**不调 sage**；专家问答**不调 kb-recall/kb-search**。
> ⚠️ DSL 表达不了的复杂查询（range 区间 / rank 排序 / exact 精确 / 多库并行）→ MCP `kb_search` 兜底，能力边界详见 `skills/kbcli/SKILL.md`。

**高频歧义词**：
> - `预算` ≠ `成本`：「预算」指立项预算金额；「成本」指实际发生的消耗费用，两者不互通。
> - `主角` 单独出现或作为查询对象出现时，优先视为星舟视频电视剧项目名，走查数链路；除非有明确语义`XX 的主角`、`男主角`、`女主角`是主演的本义。

---

## §2 已下线能力（⛔ 禁止执行）

以下能力对应的 Skill 已从本环境移除，**当前不支持**。用户提及时如实说明不可用，禁止编造流程、伪造结果或调用不存在的工具：

- 项目/草稿管理、评估单（原 `szabot-project` / `szabot-estimate`）
- 文件上传/解析/模板填写（原 `szabot-file-uploader` / `szabot-file-parser` / `szabot-file-editor`）
- 看片播放（原 `szabot-play`）、定时任务/热度监控（原 `szabot-message-send`）
- 独立网络搜索、侵权数据查询、素材/制作进度查询（原 `szabot-web-search` / `szpp-data-query` / `szstudio-cms-board` / `short-anime-data-query`）

> 注：**小说/剧本分析**若基于用户上传的文件——文件链路已下线，不可执行；若走专家问答（@expert:novel / @expert:script），照常路由 sage。

---

## §3 公共护栏

1. **禁凭记忆构造参数**：字段名、枚举值、命令参数必须从 `skills/kbcli/SKILL.md` 与 `references/` 读取，禁止凭记忆或签名猜测。
2. **影库优先**：影视综数据一律先走 kbcli 影库链路；禁止用通用网络搜索替代影库查询，外部信息仅在影库查无时作补充并注明来源。
3. **不编造**：CLI 报错/超时/返回空 → 修正参数后重试；仍失败则据已有上下文如实说明，禁止编造字段或结果。
4. **失败不放弃、也不无效重试**：同一命令禁止一字不差地重复执行（参数未变不会得到新结果）；应调整 `--text` / `--scope` 等参数后再试。
