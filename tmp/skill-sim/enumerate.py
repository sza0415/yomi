#!/usr/bin/env python3
"""枚举 kbcli PATH.json 中所有从 match_intent 到叶子 output 的路径，并对比图片输入的预期命中。"""
import json, sys
from pathlib import Path

PATH_FILE = "/Users/ziangsun/Documents/szabot/skills/kbcli/PATH.json"
path = json.loads(Path(PATH_FILE).read_text())
nodes = {n["id"]: n for n in path["nodes"]}

def leaf_paths(start_id):
    """从 start_id 起所有根→output 路径（output/fallback 视作叶子）。"""
    results = []
    def dfs(cur_id, trail, when_chain, branch_labels, expect_chain):
        if cur_id is None:
            results.append({
                "trail": list(trail),
                "when_chain": list(when_chain),
                "branch_labels": list(branch_labels),
                "expect_chain": list(expect_chain),
            })
            return
        n = nodes.get(cur_id)
        if not n:
            return
        trail.append(cur_id)
        kind = n["kind"]
        # 叶子：output / fallback
        if kind in ("output", "fallback"):
            dfs(None, trail, when_chain, branch_labels, expect_chain)
            trail.pop()
            return
        # decision：有 branches
        if n.get("branches"):
            for b in n["branches"]:
                label = b.get("label") or b.get("when") or "分支"
                when = b.get("when") or ""
                expect = (b.get("expect") or {})
                dfs(
                    b["to"],
                    trail,
                    when_chain + [when],
                    branch_labels + [label],
                    expect_chain + [expect],
                )
            trail.pop()
            return
        # 线性：按 next[0] 顺序串联
        nxt = n.get("next") or []
        for nxt_id in nxt:
            dfs(nxt_id, trail, when_chain, branch_labels, expect_chain)
        trail.pop()
    dfs(start_id, [], [], [], [])
    return results

leaves = leaf_paths("match_intent")
print(f"== kbcli PATH.json 枚举到 {len(leaves)} 条根→叶路径 ==\n")
for i, lp in enumerate(leaves, 1):
    label = " / ".join(lp["branch_labels"]) or "(直行)"
    when = " ; ".join([w for w in lp["when_chain"] if w])
    expect_types = [e.get("type", "") for e in lp["expect_chain"] if e]
    print(f"━━ 叶子路径 #{i}  ┃  分支:{label}")
    print(f"   触发条件: {when}")
    print(f"   终点预期: {' → '.join(expect_types) if expect_types else '(继承)'}")
    print(f"   节点链路: {' → '.join(lp['trail'])}")
    print()

# ====== 用图片里的输入判断应命中哪条 ======
print("=" * 60)
print("image 输入解析：")
print("  - agent: 剧本专家 → @expert:script")
print("  - text 模板: proj_id:...,scriptAnalysisId:...,scriptVersionId:...,对上下文中关于 剧本专家 的内容进行分析")
print("  - 语义: 写一篇故事摘要（专家问答/创作生成，不是查数/指标查询）")
print()

image_input_intent = "专家问答-剧本分析"
print(f">>> 图片输入的预期意图: {image_input_intent}")
print()
print(">> 在 4 条叶子路径中匹配：")
for i, lp in enumerate(leaves, 1):
    label = " / ".join(lp["branch_labels"]) or "(直行)"
    hit = "🎯 命中" if ("专家" in label or "sage" in str(lp["trail"]).lower()) else ""
    print(f"  #{i} {label:30s}  工具节点={[n for n in lp['trail'] if n.startswith('node_')]}  {hit}")
