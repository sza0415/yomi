# Skill 评审报告

- Skill 版本：`kbcli@2025-08-20`
- 用例：4，总体通过：3，失败：1

## 指标

| 指标 | 结果 |
|---|---:|
| Skill 命中率 | 100.0% |
| Path 覆盖率 | 100.0% |
| Node 覆盖率 | 100.0% |
| 路径匹配率 | 100.0% |
| 链路通过率 | 83.3% |
| 节点通过率 | 85.7% |
| 注意事项通过率 | 66.7% |
| 输出通过率 | 80.0% |

## 用例结果

| Case | 状态 | 失败节点 | 问题 |
|---|---|---|---|
| `kbcli-dsl-simple-query` | 通过 | `-` | - |
| `kbcli-script-sage-correct` | 通过 | `-` | - |
| `kbcli-script-sage-wrong-branch` | 失败 | `node_sage` | 完整执行链路缺少预期步骤 |
| `kbcli-sql-mode` | 通过 | `-` | - |

## 失败详情
### `kbcli-script-sage-wrong-branch` 剧本专家问答 - 走错分叉（误判为查数）

- `chain_step_missing`：完整执行链路缺少预期步骤（节点：`node_sage`）
- `tool_missing`：执行链路要求的工具未被调用（节点：`node_sage`）
- `notice_missed`：Skill 注意事项未被确认或遵守（节点：`查数与专家两条线互斥`）
- `node_missing`：实际路径缺少预期节点（节点：`node_sage`）
- `tool_missing`：节点要求的工具未被调用（节点：`node_sage`）
- `node_missing`：实际路径缺少预期节点（节点：`output_sage`）
- `output_type_mismatch`：最终输出类型不符合预期
- `output_content_missing`：最终输出缺少预期内容（节点：`剧本`）
