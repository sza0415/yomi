---
name: kbcli
description: 影库 CLI — 面向 AI Agent 专家模式，支持影视综数据查询、结构化 ID 查询、营销方案等服务。
user-invocable: true
---

# 影库 CLI (kbcli)

面向 AI Agent 的影视制片一体化工具，封装影库平台 20+ 核心能力。

| 场景 | 走哪条线 | 命令 |
|------|---------|------|
| 字段/指标范围召回（所有查数场景的第一步） | **kb-recall** | `kbcli kb-recall --text ... [--scope ...]` |
| **电视剧播放指标取数**（MySQL） | **kb-search（SQL 模式）** | `kbcli kb-search --domain-knowledge "影库知识库-MYSQL-24" --database <db> --sql <SQL>` |
| 专家问答（@expert:market/variety/novel/script） | **sage** | `kbcli sage ask --agent-id=... --text ...` |

> ⛔ 查数与专家两条线互斥：查数场景**不调 sage**；专家问答**不调 kb-recall/kb-search**。

### ⚠️ `kb-search` 的两种模式与能力边界

`kb-search` 有两种**互斥**模式（同时传或都不传都会报 `InvalidQuery`）：

| 模式 | 参数 | 用途 | 当前是否使用 |
|---|---|---|---|
| **SQL 模式** | `--database` + `--sql`（须同时传） | MySQL 知识库取指标 | ✅ **用于电视剧播放**（执行 md_doc 内 SQL） |
| **DSL 模式** | `--query "SELECT <字段> [WHERE ...]"` | ES 知识库查元数据 | ✅ **用于项目ID/剧集分类定位** |

**`--query` 语法**：`SELECT <字段,字段> [WHERE 字段 = "值" AND 字段 IN ("值1","值2")]`
- `SELECT` 后 = `target`；`WHERE` 后 = `condition`，只支持 `=` / `IN`，用 `AND` 连接，**值必须带引号**。

> 🚨 **`--query`（DSL 模式）的能力边界**——以下场景 CLI 表达不了，仍需 MCP `kb_search`（`mcporter call 'szabot_tools.kb_search(...)'`）：
> - ❌ 不支持 `range`（区间过滤，如「2025年播出的剧」）
> - ❌ 不支持 `rank`（排序，如「热度最高的10部」）
> - ❌ `match` 硬编码 `fuzzy`，**无法指定 `exact`**（剧名「先精确→再模糊」策略）
> - ❌ `--domain-knowledge` 只接受**单个**库，无法一次并行查多库（竞品对比需多次调用）
>
> ✅ **但「拿项目ID/剧集分类」不需要上述能力**（只用 condition + target + fuzzy），故已统一用 `--query`。
>
> ⚠️ `--query` 与 `--database/--sql` **互斥**（同传或都不传均报 `InvalidQuery`）；值须压成一行；返回结果包在 `<text>...</text>` 内。


## 📚 深度指南 (references/)
本 Skill 附带详细参考文档，覆盖复杂工作流：

| 文档 | 内容 |
|------|------|
| [`references/kb-recall.md`](references/kb-recall.md) | kb-recall 知识库字段召回模块 说明和调用示例 |
| [`references/sage.md`](references/sage.md) | sage 问答模块，包含 `专家模式` 说明和调用示例 |


## ⚠️ 重要规则
1. **强制读取参考文档** 激活本 Skill 后，**必须先读取 `references/kb-recall.md` 和 `references/sage.md`**，严格按照其中定义的子 Agent 路由表、调用方式、参数格式和重要规则执行，禁止跳过该文档凭记忆或猜测拼接命令参数
