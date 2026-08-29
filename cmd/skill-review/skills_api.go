package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ziangsun/szabot/internal/skillreview"
	"github.com/ziangsun/szabot/internal/skills"
)

// skillsAPI 承载 Skill 管理模块的后端：列出、读取、保存 SKILL.md，
// 以及从 SKILL.md 正文生成预设 Path。
//
// 设计约束（与仓库一致）：
//   - 零第三方依赖，仅用标准库；
//   - 只在 workspace/skills 目录内读写，name 经过清洗，杜绝路径穿越。
type skillsAPI struct {
	// workspace 是沙盒根，所有读写都必须落在它之下。
	workspace string
	// skillsDir 是技能目录（默认 workspace/skills），复用 skills.Loader 发现技能。
	skillsDir string
	loader    *skills.Loader
	// llm 是可选的 LLM 抽取器（据环境变量构造）。为 nil 时 Path 生成
	// 回退到规则版 derivePath。
	llm *llmExtractor
}

func newSkillsAPI(workspace, skillsDir string) *skillsAPI {
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err == nil {
		workspace = abs
	}
	if skillsDir == "" {
		skillsDir = filepath.Join(workspace, "skills")
	}
	return &skillsAPI{
		workspace: workspace,
		skillsDir: skillsDir,
		loader:    skills.NewLoader(workspace),
		llm:       newLLMExtractor(),
	}
}

// register 把 Skill 管理相关路由挂到 mux 上。
func (a *skillsAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/skills", a.handleList)
	mux.HandleFunc("/api/skill", a.handleSkill)
	mux.HandleFunc("/api/skill/paths", a.handleGenPath)
}

// skillSummary 是 /api/skills 返回给前端的精简结构，字段名与前端 index.html 对齐。
type skillSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	RelPath     string   `json:"rel_path"`
	Bins        []string `json:"bins,omitempty"`
	Env         []string `json:"env,omitempty"`
	Available   bool     `json:"available"`
}

// handleList: GET /api/skills —— 列出所有可发现的 Skill。
func (a *skillsAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	list := a.loader.List()
	out := make([]skillSummary, 0, len(list))
	for _, s := range list {
		out = append(out, skillSummary{
			Name:        s.Name,
			Description: s.Description,
			RelPath:     s.RelPath,
			Bins:        s.Requires.Bins,
			Env:         s.Requires.Env,
			Available:   true,
		})
	}
	writeJSON(w, out)
}

// handleSkill: GET/PUT /api/skill?name=xxx —— 读取或保存某个 SKILL.md 原文。
func (a *skillsAPI) handleSkill(w http.ResponseWriter, r *http.Request) {
	name, err := a.safeName(r.URL.Query().Get("name"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	path := filepath.Join(a.skillsDir, name, "SKILL.md")

	switch r.Method {
	case http.MethodGet:
		content, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, map[string]string{"name": name, "content": ""})
				return
			}
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]string{"name": name, "content": string(content)})

	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 上限 1MB，防滥用
		if err != nil {
			httpError(w, http.StatusBadRequest, "read body failed")
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]string{"name": name, "status": "saved"})

	default:
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// genPathRequest 是 /api/skill/paths 的请求体。content 可选：
// 提供则用它生成（未保存的草稿也能预览），否则回落到磁盘上的 SKILL.md。
type genPathRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// handleGenPath: /api/skill/paths
//
//	GET  ?name=xxx           读取已存储的 Path 缓存（skills/<name>/PATH.json）。
//	                         没有缓存时返回 {"cached": false}，前端据此提示去生成。
//	POST {name, content?}    生成 Path 并写入缓存；带 content 草稿时只预览、不落盘。
//	     ?engine=auto|llm|rule
//
// 生成有成本（尤其 LLM），因此结果落盘为 PATH.json，Web 加载时直接读缓存，
// 只有用户主动「生成 / 重新生成」时才重算并覆盖。
func (a *skillsAPI) handleGenPath(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleGetPath(w, r)
	case http.MethodPost:
		a.handlePostPath(w, r)
	default:
		httpError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleGetPath 读取某个 skill 已存储的 Path 缓存。
func (a *skillsAPI) handleGetPath(w http.ResponseWriter, r *http.Request) {
	name, err := a.safeName(r.URL.Query().Get("name"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := os.ReadFile(a.pathCacheFile(name))
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, map[string]any{"cached": false})
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var path skillreview.PathDefinition
	if err := json.Unmarshal(data, &path); err != nil {
		// 缓存损坏时当作未缓存，让前端重新生成。
		writeJSON(w, map[string]any{"cached": false})
		return
	}
	w.Header().Set("X-Path-Source", "cache")
	writeJSON(w, map[string]any{"cached": true, "path": path})
}

// handlePostPath 生成 Path（LLM 优先，规则版兜底），并在使用磁盘 SKILL.md
// 时把结果写入缓存。带 content 草稿（未保存的编辑）时只预览、不落盘。
func (a *skillsAPI) handlePostPath(w http.ResponseWriter, r *http.Request) {
	var req genPathRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json")
		return
	}
	name, err := a.safeName(req.Name)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	// draft=true 表示用请求体里的草稿正文生成（不落盘）；否则读磁盘 SKILL.md（结果落盘）。
	draft := strings.TrimSpace(req.Content) != ""
	content := req.Content
	if !draft {
		data, err := os.ReadFile(filepath.Join(a.skillsDir, name, "SKILL.md"))
		if err != nil {
			httpError(w, http.StatusNotFound, "SKILL.md not found")
			return
		}
		content = string(data)
	}

	// 引擎选择：engine=rule 强制规则版；engine=llm 强制 LLM（不可用则报错）；
	// 默认 auto —— 有 LLM 先试 LLM，失败自动回退规则版，保证总能出草稿。
	engine := r.URL.Query().Get("engine")
	source := "rule"
	var path skillreview.PathDefinition

	if engine != "rule" && a.llm != nil {
		p, err := a.llm.extract(r.Context(), name, content)
		if err == nil {
			path, source = p, "llm"
		} else if engine == "llm" {
			httpError(w, http.StatusBadGateway, "llm extract failed: "+err.Error())
			return
		} else {
			log.Printf("[skill-review] LLM 抽取失败，回退规则版 name=%s: %v", name, err)
		}
	} else if engine == "llm" {
		httpError(w, http.StatusBadRequest, "llm engine unavailable (set SZABOT_PROVIDER + API key)")
		return
	}

	if source == "rule" {
		path = derivePath(name, content)
	}

	// 只有基于磁盘 SKILL.md 的正式生成才落盘；草稿预览不覆盖缓存。
	if !draft {
		if err := a.writePathCache(name, path); err != nil {
			log.Printf("[skill-review] 写入 Path 缓存失败 name=%s: %v", name, err)
		}
	}

	w.Header().Set("X-Path-Source", source)
	writeJSON(w, path)
}

// pathCacheFile 返回某 skill 的 Path 缓存文件路径（skills/<name>/PATH.json）。
func (a *skillsAPI) pathCacheFile(name string) string {
	return filepath.Join(a.skillsDir, name, "PATH.json")
}

// writePathCache 把生成好的 Path 以缩进 JSON 落盘，供下次直接加载。
func (a *skillsAPI) writePathCache(name string, path skillreview.PathDefinition) error {
	data, err := json.MarshalIndent(path, "", "  ")
	if err != nil {
		return err
	}
	file := a.pathCacheFile(name)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644)
}

// safeName 清洗 skill 名，只允许字母数字、连字符、下划线，杜绝路径穿越。
func (a *skillsAPI) safeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if !safeNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid skill name")
	}
	return name, nil
}

var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ---- 规则版 Path 生成：从 SKILL.md 正文推导执行路径 ----
//
// 与前端 derivePathFromMd 的启发式保持一致，但以后端为权威：
//
//	意图匹配（模型据 description 命中该 Skill）
//	  → 先读 references（若正文要求）
//	  → 工具调用（命令速查里的可执行脚本）
//	  → 结果校验 / 注意事项
//	  → 异常兜底
//	  → 输出
//
// 说明：szabot 没有"统筹 Skill"，意图识别由模型基于各 Skill 的 description
// 自主完成、Skill 之间是扁平平级的。因此入口节点代表的是"模型的意图匹配"，
// 而非某个统筹 Skill 的执行。
var (
	triggerRe = regexp.MustCompile(`@[a-zA-Z]+:[a-zA-Z\-]+`)
	refRe     = regexp.MustCompile(`references/[\w.\-]+`)
	// bash <path>/<tool>：抓命令速查里的可执行脚本名。
	toolRe = regexp.MustCompile(`bash\s+([\w./\-]+/([\w\-]+))`)
)

func derivePath(name, md string) skillreview.PathDefinition {
	nodes := make([]skillreview.NodeDefinition, 0, 6)

	// 1) 意图匹配：模型据 description 命中该 Skill。
	triggers := dedup(triggerRe.FindAllString(md, -1))
	entry := fmt.Sprintf("模型据 description 命中 %s", name)
	if len(triggers) > 0 {
		entry += "（触发词 " + strings.Join(limit(triggers, 3), " / ") + "）"
	}
	nodes = append(nodes, skillreview.NodeDefinition{
		ID: "match_intent", Kind: skillreview.NodeInput, Condition: entry, Required: true,
	})

	// 2) 前置：正文要求"必须先读取 references/xxx"。
	if (strings.Contains(md, "必须先读取") || strings.Contains(md, "强制读取")) && refRe.MatchString(md) {
		nodes = append(nodes, skillreview.NodeDefinition{
			ID: "read_reference", Kind: skillreview.NodeValidation,
			Condition: "先读取 " + refRe.FindString(md), Required: true,
		})
	}

	// 3) 工具调用：命令速查里的 bash 脚本。
	seen := map[string]bool{}
	for _, m := range toolRe.FindAllStringSubmatch(md, -1) {
		tool := m[2]
		if seen[tool] {
			continue
		}
		seen[tool] = true
		nodes = append(nodes, skillreview.NodeDefinition{
			ID: "run_" + tool, Kind: skillreview.NodeTool, Tool: tool,
			Condition: "执行 " + tool, Required: true,
		})
		if len(seen) >= 4 {
			break
		}
	}

	// 4) 结果校验 / 注意事项。
	if strings.Contains(md, "重要规则") || strings.Contains(md, "禁止") || strings.Contains(md, "不得") {
		nodes = append(nodes, skillreview.NodeDefinition{
			ID: "verify_result", Kind: skillreview.NodeValidation,
			Condition: "按重要规则校验结果，禁止伪造", Required: true,
			Notes: []string{"不得把模拟结果描述成真实联网结果"},
		})
	}

	// 5) 异常兜底。
	if strings.Contains(md, "异常处理") || strings.Contains(md, "重试") || strings.Contains(md, "退出") {
		nodes = append(nodes, skillreview.NodeDefinition{
			ID: "handle_error", Kind: skillreview.NodeFallback,
			Condition: "工具失败时补参重试 / 友好提示", Required: false,
		})
	}

	// 6) 输出。
	nodes = append(nodes, skillreview.NodeDefinition{
		ID: "produce_output", Kind: skillreview.NodeOutput,
		Condition: "产出结构化结果", Required: true,
	})

	return skillreview.PathDefinition{
		PathID:          "path_" + sanitizeID(name),
		Name:            name + " 完整路径",
		EntryConditions: limit(triggers, 3),
		Nodes:           nodes,
		Exit:            "produce_output",
	}
}

func dedup(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}

func limit(items []string, n int) []string {
	if len(items) > n {
		return items[:n]
	}
	return items
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}
