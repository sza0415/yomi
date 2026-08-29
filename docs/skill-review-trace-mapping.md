# 真实运行 Trace 映射

`skill-review` 不修改 yomi 的运行时 Trace。它可以在评审侧读取一个真实运行产生的 JSONL 文件，再按映射规则投影成现有的 `skillreview.Run`。

映射文件示例：

```json
{
  "skill": "kbcli",
  "rules": [
    {
      "event_type": "tool.execution.started",
      "tool_name": "read_file",
      "node_id": "read-skill",
      "kind": "validation",
      "source": "runtime-trace"
    },
    {
      "event_type": "tool.execution.started",
      "tool_name": "bash",
      "node_id": "execute-kbcli",
      "kind": "tool"
    }
  ]
}
```

运行评审：

```bash
go run ./cmd/skill-review \
  -cases cases.json \
  -trace sessionlogs/traces/<run-file>.jsonl \
  -trace-mapping mapping.json \
  -trace-case-id case-001 \
  -markdown report.md
```

规则按事件顺序匹配。`event_type` 和 `node_id` 必填；指定 `tool_name` 时还会检查运行时事件中的 `data.tool_name`。没有命中规则的运行事件仍保留在原始 Trace 中，但不会被推断成 Skill 节点。评审结果中的 `TraceEvent` 会记录 `runtime_type`、`sequence` 和 `source`，便于回溯到原始运行。
