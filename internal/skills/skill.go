// Package skills 实现 szabot 的技能（Skill）系统。
//
// 设计理念：渐进式披露（Progressive Disclosure），把"技能知识"按访问频率分三层，
// 避免一次性把所有知识塞进 context 而挤占宝贵的窗口：
//
//	L1 元数据（name + description + 路径）：进程启动时全量常驻 system prompt。
//	    量级极小（每个技能几十 token），且固定不变 —— KV Cache 友好。
//	L2 SKILL.md 正文（核心流程）：任务触发时，由 agent 用现成的 read_file
//	    读取 L1 给出的路径，按需加载。本包不提供专门的 load 工具。
//	L3 子资源（references/scripts/assets）：由 agent 在正文引导下自行决定读不读，
//	    脚本甚至可以直接执行而不必读入 context。
//
// 关键约束：szabot 的 read_file 工具被限制在 workspace 内且只接受相对路径，
// 因此技能目录必须位于 workspace 下（默认 skills/），L1 摘要里给出的路径
// 也必须是 workspace 相对路径，这样 agent 才能用现成工具读到，无需新增工具。
package skills

import (
	"encoding/json"
	"strings"
)

// Requires 声明一个技能运行所需的外部依赖。
// 在生成 L1 摘要时做校验，不满足则在摘要里标注 (unavailable: ...)，
// 避免 agent 触发后才发现跑不了、白白浪费一轮对话。
type Requires struct {
	Bins []string `json:"bins"`
	Env  []string `json:"env"`
}

// Meta 是一个技能的 L1 元数据，从 SKILL.md 的 frontmatter 解析而来。
type Meta struct {
	Name        string   // 技能名（等于目录名）
	Description string   // 唯一的触发信号：既要写"做什么"，也要写"何时用"
	Requires    Requires // 外部依赖（可选）
	Always      bool     // 为 true 时正文强制常驻，不走按需加载
}

// Skill 是磁盘上的一个技能。
type Skill struct {
	Meta
	// RelPath 是 SKILL.md 相对 workspace 的路径（如 "skills/github/SKILL.md"），
	// 直接写进 L1 摘要供 agent 用 read_file 读取。
	RelPath string
	// AbsPath 是 SKILL.md 的绝对路径，供本包内部读取正文用。
	AbsPath string
}

// metadataField 对应 frontmatter 里 metadata 字段的 JSON 结构。
// 兼容 nanobot：优先取 nanobot 命名空间，其次 openclaw。
type metadataField struct {
	Requires Requires `json:"requires"`
	Always   bool     `json:"always"`
}

// parseMetadata 解析 frontmatter 里的 metadata（JSON 字符串或对象）。
func parseMetadata(raw string) metadataField {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return metadataField{}
	}
	// metadata 形如 {"nanobot":{...}} 或直接 {"requires":{...}}。
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return metadataField{}
	}
	// 优先 nanobot / openclaw 命名空间，回退到顶层。
	for _, key := range []string{"nanobot", "openclaw"} {
		if inner, ok := envelope[key]; ok {
			var m metadataField
			if json.Unmarshal(inner, &m) == nil {
				return m
			}
		}
	}
	var m metadataField
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}
