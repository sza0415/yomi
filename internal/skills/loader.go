package skills

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Loader 负责发现技能并生成注入 context 的各层内容。
//
// 技能来源两处，workspace 优先级更高（同名覆盖 builtin），
// 方便用户在不改源码的前提下覆盖内置技能。
type Loader struct {
	workspace   string // workspace 绝对路径，L1 摘要里的路径相对它计算
	roots       []string
	disabled    map[string]bool
	enabled     map[string]bool // 非 nil 时只允许指定技能
	skillSubdir string          // workspace 下技能目录名，默认 "skills"
}

// Option 配置 Loader。
type Option func(*Loader)

// WithBuiltinDir 追加一个内置技能根目录（可被 workspace 同名技能覆盖）。
func WithBuiltinDir(dir string) Option {
	return func(l *Loader) {
		if strings.TrimSpace(dir) != "" {
			l.roots = append(l.roots, dir)
		}
	}
}

// WithDisabled 禁用指定技能。
func WithDisabled(names ...string) Option {
	return func(l *Loader) {
		for _, n := range names {
			l.disabled[n] = true
		}
	}
}

// WithEnabled restricts discovery to the named skills. An empty name list
// keeps the default (all discovered skills) behavior.
func WithEnabled(names ...string) Option {
	return func(l *Loader) {
		if len(names) == 0 {
			return
		}
		l.enabled = map[string]bool{}
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				l.enabled[n] = true
			}
		}
	}
}

// NewLoader 创建一个 Loader。workspace 是 read_file 的沙盒根，
// workspace/skills 是默认的用户技能目录（优先级最高）。
func NewLoader(workspace string, opts ...Option) *Loader {
	l := &Loader{
		workspace:   workspace,
		disabled:    map[string]bool{},
		skillSubdir: "skills",
	}
	// workspace 技能目录排在最前 —— 后续 collect 用"先到先得"实现同名覆盖。
	l.roots = append(l.roots, filepath.Join(workspace, l.skillSubdir))
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// List 返回所有可发现的技能，workspace 同名技能覆盖 builtin。
func (l *Loader) List() []Skill {
	seen := map[string]bool{}
	var result []Skill
	for _, root := range l.roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // 目录不存在则跳过，属正常情况
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if seen[name] || l.disabled[name] || (l.enabled != nil && !l.enabled[name]) {
				continue
			}
			abs := filepath.Join(root, name, "SKILL.md")
			content, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			meta, ok := parseFrontmatter(string(content))
			if !ok {
				continue
			}
			if strings.TrimSpace(meta.Name) == "" {
				meta.Name = name
			}
			seen[name] = true
			result = append(result, Skill{
				Meta:    meta,
				AbsPath: abs,
				RelPath: l.relPath(abs),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// relPath 把绝对路径转成 workspace 相对路径（用 / 分隔，供 read_file 使用）。
// 若技能在 workspace 之外（如内置目录），返回绝对路径作为兜底提示。
func (l *Loader) relPath(abs string) string {
	rel, err := filepath.Rel(l.workspace, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	return filepath.ToSlash(rel)
}

// Summary 生成 L1 摘要（注入 system prompt）。
// 每行形如： - **name** — description  `skills/name/SKILL.md`
// 不可用的技能标注 (unavailable: ...)，仍然列出以便 agent 提示用户安装。
func (l *Loader) Summary() string {
	list := l.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range list {
		if s.Always {
			continue // always 技能正文已常驻，不再重复列进摘要
		}
		if missing := missingRequirements(s.Requires); missing != "" {
			fmt.Fprintf(&b, "- **%s** — %s (unavailable: %s)  `%s`\n",
				s.Name, s.Description, missing, s.RelPath)
		} else {
			fmt.Fprintf(&b, "- **%s** — %s  `%s`\n", s.Name, s.Description, s.RelPath)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// AlwaysBodies 返回所有 always=true 且依赖满足的技能正文（已剥离 frontmatter）。
// 这些正文在装配时直接拼进 system prompt，不走按需加载。
func (l *Loader) AlwaysBodies() string {
	var parts []string
	for _, s := range l.List() {
		if !s.Always {
			continue
		}
		if missingRequirements(s.Requires) != "" {
			continue
		}
		content, err := os.ReadFile(s.AbsPath)
		if err != nil {
			continue
		}
		body := stripFrontmatter(string(content))
		parts = append(parts, fmt.Sprintf("### Skill: %s\n\n%s", s.Name, body))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// missingRequirements 返回缺失依赖的可读描述，全部满足时返回空串。
func missingRequirements(r Requires) string {
	var missing []string
	for _, bin := range r.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, "CLI: "+bin)
		}
	}
	for _, env := range r.Env {
		if os.Getenv(env) == "" {
			missing = append(missing, "ENV: "+env)
		}
	}
	return strings.Join(missing, ", ")
}

// stripFrontmatter 去掉 SKILL.md 开头的 YAML frontmatter，只留正文。
func stripFrontmatter(content string) string {
	trimmed := strings.TrimLeft(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---") {
		return content
	}
	rest := trimmed[len("---"):]
	rest = strings.TrimLeft(rest, "\r\n")
	end := findClosingFence(rest)
	if end < 0 {
		return content
	}
	body := rest[end:]
	body = strings.TrimPrefix(strings.TrimLeft(body, "\r\n"), "---")
	return strings.TrimLeft(body, "\r\n")
}
