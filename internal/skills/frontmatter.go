package skills

import (
	"strings"
)

// parseFrontmatter 从 SKILL.md 内容里解析出 Meta。
//
// SKILL.md 以 YAML frontmatter 开头：
//
//	---
//	name: github
//	description: "用 gh CLI 操作 GitHub。处理 issue、PR、CI 时使用。"
//	metadata: {"requires":{"bins":["gh"]}}
//	---
//	# 正文...
//
// 为保持 szabot 零外部依赖，这里不引入完整 YAML 库，只做一个受限解析器：
//   - 仅支持 "key: value" 的单行键值对（技能 frontmatter 的实际形态）；
//   - value 可用单/双引号包裹，会被剥离；
//   - metadata 的 value 是一段 JSON，交给 parseMetadata 处理。
//
// 返回的 bool 表示是否成功解析出 frontmatter（缺少 --- 边界则为 false）。
func parseFrontmatter(content string) (Meta, bool) {
	content = strings.TrimLeft(content, "\ufeff") // 去掉可能的 BOM
	if !strings.HasPrefix(content, "---") {
		return Meta{}, false
	}

	// 定位第二个 --- 边界（frontmatter 结束）。
	rest := content[len("---"):]
	rest = strings.TrimLeft(rest, "\r\n")
	end := findClosingFence(rest)
	if end < 0 {
		return Meta{}, false
	}
	block := rest[:end]

	fields := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		fields[key] = unquote(value)
	}

	meta := Meta{
		Name:        fields["name"],
		Description: fields["description"],
	}
	if md, ok := fields["metadata"]; ok {
		parsed := parseMetadata(md)
		meta.Requires = parsed.Requires
		meta.Always = parsed.Always
	}
	// 顶层 always（不经 metadata 包裹）也支持，方便简单技能。
	if v, ok := fields["always"]; ok && (v == "true" || v == "yes") {
		meta.Always = true
	}
	return meta, true
}

// findClosingFence 返回下一处独占一行的 "---" 的起始下标（相对 s）。
func findClosingFence(s string) int {
	offset := 0
	for _, line := range strings.SplitAfter(s, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) == "---" {
			return offset
		}
		offset += len(line)
	}
	return -1
}

// unquote 剥离一对包裹的单/双引号。
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
