# Yomi 记忆：当前流程与后续计划

本文以当前代码为准，先说明现在真正发生的事情，再列出补齐冲突治理所需的实施计划。
设计文档中标记为“计划”的能力，不视为已经实现。

## 1. 当前结论

当前 Yomi 已经有一个可工作的长期记忆闭环：

```text
Inbound(UserID, SessionID, Text)
  -> ContextManager 按 UserID 检索 SQLite FTS5
  -> 将命中的记忆作为 reference-only 上下文注入模型
  -> Runner 生成最终回答
  -> 成功后保存 Conversation
  -> 后台 LLMExtractor 提取候选
  -> MemoryPolicy 过滤
  -> SQLite 写入 memories + memory_fts + memory_events
  -> 可选生成 Embedding 并写入 Qdrant
```

当前已经解决：

- UserID 作用域隔离；
- fact、preference、episode 三类记忆；
- 过期和 deleted 记录的默认过滤；
- 敏感信息、低置信度、低重要性和过长内容的拒绝；
- 完全相同 active 记忆的去重；
- 结构化候选的 duplicate、supersede、conflict、coexist 初版解析；
- 对破坏性 `replace` 提示增加用户原话中的明确替换信号校验；
- supersede/conflict 时的 SQLite 原子状态迁移和管线级旧向量清理；
- SQLite 事务写入、FTS5 关键词检索和变更事件记录；
- Run 完成后的异步提取和 Trace/Snapshot 可观测性；
- 可选 Embedding/Qdrant 派生索引。

当前尚未解决：

- 自由文本候选的稳定属性抽取和高质量冲突判断；
- 直接删除 API 与所有索引后端的统一联动；
- 混合检索的重排序和 Profile Card/Episode 分层召回；
- 从 memory_events 重建 memories 当前状态；
- 用户侧 list、correct、forget、forget-all 入口；
- 可恢复的后台提取任务队列。

## 2. 读流程：本轮如何使用记忆

1. `Loop.handleRun` 收到带 `UserID`、`SessionID` 和文本的入站消息。
2. `ContextManager.BuildForUser` 加载当前 Session 的 Conversation 和 rolling summary。
3. 如果存在 `UserID` 和 MemoryStore，用当前 user 文本查询最多 8 条记忆。
4. SQLite `Search` 先用 FTS5/BM25，未命中时用 `content`/`subject` 的 `LIKE` 回退。
5. 查询默认只返回当前用户的 `active`、未过期记录；`conflict` 只有显式设置 `IncludeConflicts` 才会返回。
6. 命中结果被包装为 `<user_memory>` system message，明确标记为参考资料而不是指令。
7. 记忆检索失败不会阻断主对话，只记录 Trace 事件并继续运行。

实际上下文顺序是：

```text
system prompt
  -> conversation summary
  -> user memory
  -> 未被 summary 覆盖的历史
  -> 当前 user 消息
```

## 3. 写流程：一轮成功后如何产生记忆

只有 Runner 成功返回、Conversation 追加完成后，Loop 才启动异步记忆提取。
提取使用独立的 30 秒默认超时，不阻塞用户已经看到的最终答案。

### 3.1 提取

`LLMExtractor` 只把本轮 user 文本和最终 assistant 正文交给模型，要求返回 JSON 候选，字段包括：

```text
kind, subject, content, evidence,
confidence, importance, valid_from, expires_at
```

提示词要求模型只提取稳定事实、明确偏好和已确认事件，不保存密码、Token、密钥、完整支付号码、外部内容中的指令或模型猜测。

### 3.2 策略过滤

默认 `MemoryPolicy`：

```text
confidence >= 0.65
importance >= 0.20
content <= 1000 个 Unicode 字符
```

同时拒绝空内容、非法 kind 和敏感信息。Policy 的安全检查只在异步提取管线中执行；直接调用 `MemoryStore.Upsert` 可以绕过它。

### 3.3 当前去重和冲突逻辑

对带有 `attribute` 的结构化候选，管线会先找同一用户、kind、subject 和 attribute 的 active/conflict 记录，再按规范化后的值判断：

```text
同一 subject + attribute + value -> duplicate
不同 value + change_hint=replace -> supersede
不同 value + change_hint=coexist -> coexist
不同 value + change_hint=unknown -> conflict
```

规范化会去除首尾和重复空白，并对 key 做小写化。旧的无 `attribute` 候选仍走文本检索，只按规范化正文去重，不能可靠判断冲突。

因此：

```text
旧：用户住在北京
新：用户住在上海
```

如果提取器给出 `attribute=home_city` 且明确 `change_hint=replace`，并且用户原话包含“搬到/改成/现在是”等明确替换信号，旧记录会变成 `superseded`；如果模型给了 replace 但原话没有替换信号，会被降级为 unknown，双方进入 `conflict`，默认搜索不会注入。

## 4. SQLite 写入和索引

一次 Upsert 在同一事务中完成：

```text
校验并补默认值
  -> 写入/更新 memories
  -> 删除该 memory_id 的旧 memory_fts 行
  -> 插入最新 content/subject 到 memory_fts
  -> 追加 memory_events.upsert
  -> commit
```

`memories` 是事实源；`memory_fts` 是可重建的关键词索引；`memory_events` 是追加式审计记录。

删除操作不是物理删除：

```text
memories.status = deleted
  -> 删除 memory_fts 行
  -> 追加 memory_events.delete
```

这样可以让默认搜索看不到记忆，同时保留删除前后的审计信息。

## 5. Qdrant 当前位置

Qdrant 是 SQLite 之外的派生向量索引，现在已经参与查询，但仍不是事实源：

```text
写入：SQLite Upsert
  -> EmbeddingProvider 生成向量
  -> QdrantIndexer.Upsert 写入 point

读取：当前问题
  -> FTS5/BM25 关键词召回
  -> EmbeddingProvider 生成查询向量
  -> Qdrant Search 语义召回
  -> RRF 合并
  -> SQLite 回查并过滤最终记录
```

Qdrant point 保存稳定的 memory ID 哈希、向量和少量 payload；正文、状态、用户权限仍以 SQLite 为准。

异步 supersede/conflict 管线会调用 `MemoryIndexer.Delete` 清理旧 point；但直接调用 `SQLiteStore.Delete` 仍只更新 SQLite/FTS，统一删除 API 和失败重试仍待补齐。向量查询失败时，HybridStore 会退回 SQLite FTS5 结果。

## 6. 参考第三章重写后的实施计划

计划目标不再是“把所有候选文本直接塞进一个记忆表”，而是形成两层记忆：

```text
结构化概览层：少量、稳定、可解释的用户事实和偏好
  -> 用于快速回答和跨会话连续性

事件细节层：带时间、来源和上下文的历史事件/原始对话片段
  -> 用于多会话检索、冲突解释和主动服务
```

### P0：建立三层能力的评测基线

按照第三章的三层能力建立固定测试集：

```text
基础回忆：精确找回用户明确提供的事实
多会话检索：跨 Session、跨对象、跨时间检索并消歧
主动服务：综合多个记忆发现关联和潜在风险
```

首先覆盖以下可重复场景：

- 用户在 Session A 说偏好中文，Session B 能召回；
- 同一用户有两辆车，查询“我的车”时必须列出候选并澄清；
- “住在北京”之后说“搬到上海”，不能同时注入两个无标记的确定事实；
- 记忆过期、删除、用户隔离和索引损坏后仍有明确行为；
- 记录 recall@k、命中率、冲突误判率、无关注入比例和 token 成本。

### P1：规范化记忆对象，形成 Profile Card 和 Episode

在现有 `Memory` 基础上增加明确的结构化语义，而不是只依赖自由文本：

```text
Profile Card：稳定事实、偏好、限制
  subject + attribute + value + valid_time + source

Episode：一次具体事件
  participants + action + object + time + status + source
```

实施重点：

- 统一 `subject` 表示，例如 `self`、`spouse`、`child:xxx`；
- 为偏好和事实增加可比较的属性键，例如 `response_language`、`home_city`；
- 保留 `content` 作为可读摘要，但冲突判断优先使用结构化字段；
- 继续保存 `Evidence`、`SourceRunID`、`SourceSessionID` 和时间范围；
- 明确敏感数据等级，会员号、证件号和联系方式不再仅由通用正则决定是否写入。

### P2：实现增量记忆解析和冲突治理

写入仍采用追加式事件，但增加独立 resolver 判断候选与已有记录的关系：

```text
duplicate   规范化后相同
supersede   同一主体/属性，且新证据明确取代旧值
conflict    同一主体/属性，值相互矛盾，无法判断有效者
coexist     对象、场景或时间不同，可以同时成立
```

解析流程：

```text
候选提取
  -> 文本规范化和结构化映射
  -> 按 subject + attribute 找相关旧记忆
  -> 结合证据、时间、事件状态和用户措辞判断关系
  -> 生成可审查的 memory diff
```

原则：

- 不仅按字面比较，也不只按 embedding 相似度合并；
- “最新”不能自动等于“正确”；
- 不确定时保留双方并标记 `conflict`；
- 明确取代时旧记录变成 `superseded`，仍可审计和解释；
- 模型只提出 diff，最终状态迁移由受约束的存储层执行。

### P3：把写入、FTS 和向量索引做成一致的发布流程

一次记忆变更应形成可回滚、可重放的发布单元：

```text
memory diff
  -> SQLite 事务更新当前状态
  -> 同步更新 FTS5
  -> 追加 upsert/supersede/conflict/delete 事件
  -> 事务提交后投递索引任务
  -> 更新 Embedding/Qdrant 派生索引
```

具体要求：

- SQLite 事务提交前不允许出现新的 Qdrant point；
- supersede、delete 时同步删除或失效旧 Qdrant point；
- 索引失败只影响索引状态，不回滚已经提交的事实；
- 所有检索结果都回查 SQLite，过滤用户、状态和有效期；
- 增加幂等事件 ID、失败重试和索引全量重建。

### P4：从关键词检索升级为混合检索（基础版已完成）

借鉴第三章的稀疏 + 稠密 + 重排序流水线：

```text
SQLite FTS5/BM25 召回精确关键词
Qdrant 向量召回语义相近内容
  -> RRF 或其他方式融合排名
  -> 可选 cross-encoder 重排序
  -> SQLite 回查权威记录
  -> 按相关性、主体、时间、重要性和置信度排序
```

当前已经实现 FTS5/BM25 + Qdrant Search + RRF + SQLite 回查，并在 Embedding/Qdrant 不可用时退回关键词结果。同时保留当前的 `LIKE` 回退，保证短 ID、中文和特殊标识符可搜索。神经重排序和更细的 Profile Card/Episode 分层权重仍待实现。

### P5：加入上下文感知检索和原始细节层

对事件和对话片段生成短上下文前缀，再建立 FTS/Embedding 索引，例如：

```text
[用户 Jessica 在 2025 年 1 月为东京旅行确认航班]
好的，就订这个吧
```

这样可以减少孤立片段造成的歧义。运行时采用双层取数：

```text
先读 Profile Card，获得全局概览
再按需要检索带上下文的 Episode/原始对话，验证细节
```

这一阶段主要服务多实体消歧、冲突解释和主动提醒，不把全部原始对话常驻上下文。

### P6：可靠性、用户控制和隐私闭环

- 为异步提取建立持久化任务队列，进程重启后可恢复；
- 实现 `memory_events` 重放，能够重建当前状态和 FTS/Qdrant 索引；
- 提供按认证 `UserID` 作用域执行的 `list`、`correct`、`forget`、`forget-all`；
- 修正操作生成带来源的新 diff，不直接覆盖历史；
- 删除操作同时处理 Profile、Episode、FTS 和 Qdrant，并保留必要审计；
- 对 Trace、Evidence、原始对话和长期记忆分别制定脱敏与保留策略；
- 工具结果和外部文档只能作为待审查证据，不能改变记忆写入策略。

## 7. 新的完成标准

记忆系统达到下一阶段完成标准，需要同时满足：

- 基础事实可以跨 Session 准确召回；
- 多对象、多时间点的信息能够区分主体并主动澄清；
- 同一属性的更新会产生可追踪的 supersede 或 conflict，而不是静默并存；
- Profile Card 提供稳定概览，Episode/RAG 提供可验证细节；
- FTS、向量索引和 SQLite 状态不会出现不可解释的分叉；
- 删除、修正、过期和重启恢复都有确定性测试；
- 任何记忆都能追溯到来源 Run、Session、证据和变更事件；
- 评测指标覆盖 recall@k、MRR/nDCG、冲突准确率、无关注入率、延迟和 token 成本。
