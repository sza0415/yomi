// overview 是 szabot 的「项目地图」：把 README 与 docs/ 里的长文蒸馏成一个
// 可视化 Web 工作台，帮助任何人（包括未来的自己）在几分钟内看懂项目。
//
// 与 skill-review 一样，它是纯本地、零外部依赖的工具：
//   - 前端单页用 go:embed 打进二进制，依旧是"一个可执行文件"；
//   - 不执行任何 Skill、不依赖 Docker；
//   - 技能数据是实时的：复用 internal/skills 的 Loader 读取 skills/ 目录，
//     展示当前实际生效的 L1/L2/L3 结构，而不是文档里的历史快照。
//
// 启动：
//
//	go run ./cmd/overview              # 默认 :8091
//	go run ./cmd/overview -addr :9000 -workspace .
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ziangsun/szabot/internal/skills"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8091", "HTTP 监听地址")
	workspace := flag.String("workspace", ".", "workspace 根目录（读取其中的 skills/ 与 docs/）")
	flag.Parse()

	ws, err := filepath.Abs(*workspace)
	if err != nil {
		log.Fatalf("resolve workspace: %v", err)
	}

	mux := http.NewServeMux()
	// embed 根下文件位于 web/ 子目录，取子 FS 让 "/" 直接命中 index.html。
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed sub fs: %v", err)
	}
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/overview", handleOverview(ws))
	mux.HandleFunc("GET /api/docs", handleDocsIndex(ws))
	mux.HandleFunc("GET /api/doc", handleDocContent(ws))

	fmt.Printf("overview server listening on %s (workspace: %s)\n", displayURL(*addr), ws)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func displayURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

// ---------- /api/overview：实时技能状态 + 项目规模 ----------

type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	RelPath     string `json:"relPath"`
	Always      bool   `json:"always"`
	Unavailable string `json:"unavailable,omitempty"`
}

type overviewData struct {
	Skills []skillInfo `json:"skills"`
	// L1Summary 是当前实际会注入 system prompt 的技能摘要（L2 技能的触发入口）。
	L1Summary string `json:"l1Summary"`
	// AlwaysBodiesBytes 是 always 技能常驻正文的体积（字节）。
	AlwaysBodiesBytes int `json:"alwaysBodiesBytes"`
	Stats             struct {
		GoFiles int `json:"goFiles"`
		Docs    int `json:"docs"`
		Skills  int `json:"skills"`
		Tools   int `json:"tools"`
	} `json:"stats"`
}

func handleOverview(ws string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		loader := skills.NewLoader(ws)

		data := overviewData{Skills: []skillInfo{}}
		for _, s := range loader.List() {
			info := skillInfo{
				Name:        s.Name,
				Description: s.Description,
				RelPath:     s.RelPath,
				Always:      s.Always,
			}
			data.Skills = append(data.Skills, info)
		}
		data.L1Summary = loader.Summary()
		data.AlwaysBodiesBytes = len(loader.AlwaysBodies())
		data.Stats.Skills = len(data.Skills)
		data.Stats.Tools = 12 // 注册表见 README 工具箱：read/write/edit/list_dir/glob/grep/web_fetch/todo_write/ask_user_question/web_search/bash/python
		data.Stats.Docs = countMarkdown(filepath.Join(ws, "docs"))
		data.Stats.GoFiles = countByExt(ws, ".go", map[string]bool{
			"node_modules": true, ".git": true, "tmp": true, "sessionlogs": true,
		})

		writeJSON(w, data)
	}
}

// ---------- /api/docs：文档索引（自动扫描 + 人工摘要） ----------

type docInfo struct {
	Name       string `json:"name"` // 相对 docs/ 的路径，如 "harness/agent-status-line.md"
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Group      string `json:"group"` // 架构流程 / Skill 评审 / Harness / 其他
	Bytes      int64  `json:"bytes"`
	HasMermaid bool   `json:"hasMermaid"`
}

// docMeta 是对每篇文档的一句话摘要；缺失时回退到文件首个标题。
var docMeta = map[string]struct{ Summary, Group string }{
	"cli-message-flow.md":                                      {"从命令行敲下一行字到终端打印回复的完整链路：启动装配、三个 goroutine、一轮消息的流转与生命周期。", "架构流程"},
	"function-calling-flow.md":                                 {"Function Calling 全链路：工具注册白名单、模型请求工具、Runner 校验执行、结果回传直到最终答案。", "架构流程"},
	"session-and-streaming.md":                                 {"Session 历史持久化（jsonl 落盘、KV Cache 友好）与 SSE 流式回复两项核心改动的完整归档。", "架构流程"},
	"reasoning-and-tool-events.md":                             {"把推理过程（reasoning）与工具调用/结果也纳入事件流：实时推送给 channel，并完整落盘到 session。", "架构流程"},
	"tools-integration-flow.md":                                {"工具箱扩展设计：六件套文件工具 + Docker 沙盒 bash/python，全程不改核心循环（Core stays small 的体现）。", "架构流程"},
	"skill-review-plan.md":                                     {"Skill 评审系统实现计划：为什么要从「一个总分」走向「可观测路径」，模块与指标如何设计。", "Skill 评审"},
	"skill-review-ui-redesign.md":                              {"评审工作台的界面重设计：Skill 管理 → 测试用例构建 → 评估与对比的三模块操作链路。", "Skill 评审"},
	"skill-execution-path-review.md":                           {"Skill 执行路径评审的技术汇总：需求背景、设计难点、整体架构，含 L1/L2/L3 三层渐进式披露模型。", "Skill 评审"},
	"resume-project-description.md":                            {"项目的简历描述素材：核心工作、亮点、面试讲解主线与一页精简版。", "其他"},
	"harness/agent-status-line.md":                             {"Agent 状态栏：每轮调用 LLM 前把任务清单与进度临时注入上下文末尾，让 Agent 自我感知与调节。", "Harness"},
	"harness/ask-user-question-flow.md":                        {"ask_user_question 工具：任务中途暂停、向用户提问、等回答后再继续，及 Web 端提问卡片交互。", "Harness"},
	"harness/disconnect-cancellation.md":                       {"Web 端显式取消任务并向下游传播 context；客户端断连不影响 Run。", "Harness"},
	"harness/streaming-passthrough-and-sidecar-persistence.md": {"直穿 + 旁路：同一份 LLM 输出分两路——实时推流给前端、聚合成完整消息持久化。", "Harness"},
}

func handleDocsIndex(ws string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		root := filepath.Join(ws, "docs")
		var docs []docInfo

		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil // 目录不存在或非 md 都静默跳过
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)

			info, _ := d.Info()
			title, hasMermaid := probeDoc(path)
			meta, ok := docMeta[rel]
			if !ok {
				meta.Group, meta.Summary = "其他", ""
			}
			if title == "" {
				title = rel
			}
			if meta.Summary == "" {
				meta.Summary = "（暂无摘要）"
			}
			docs = append(docs, docInfo{
				Name: rel, Title: title, Summary: meta.Summary, Group: meta.Group,
				Bytes: info.Size(), HasMermaid: hasMermaid,
			})
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if docs == nil {
			docs = []docInfo{}
		}
		sort.Slice(docs, func(i, j int) bool {
			gi, gj := groupOrder(docs[i].Group), groupOrder(docs[j].Group)
			if gi != gj {
				return gi < gj
			}
			return docs[i].Name < docs[j].Name
		})
		writeJSON(w, docs)
	}
}

func groupOrder(g string) int {
	switch g {
	case "架构流程":
		return 0
	case "Skill 评审":
		return 1
	case "Harness":
		return 2
	default:
		return 3
	}
}

// probeDoc 提取文档首个标题行（优先 # 一级，回退 ## 二级），并探测是否含 mermaid 图。
func probeDoc(path string) (title string, hasMermaid bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	hasMermaid = strings.Contains(string(b), "```mermaid")
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "# ") {
			return strings.TrimPrefix(t, "# "), hasMermaid
		}
	}
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			return strings.TrimPrefix(t, "## "), hasMermaid
		}
	}
	return "", hasMermaid
}

// ---------- /api/doc?name=：读取单篇文档正文（限定在 docs/ 内） ----------

func handleDocContent(ws string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing name", http.StatusBadRequest)
			return
		}
		// 只允许 docs/ 下的 .md，清洗掉任何路径穿越。
		if strings.Contains(name, "..") || !strings.HasSuffix(name, ".md") {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		path := filepath.Join(ws, "docs", filepath.FromSlash(name))
		// 二次校验：解析后的路径必须仍在 docs/ 内。
		if rel, err := filepath.Rel(filepath.Join(ws, "docs"), path); err != nil || strings.HasPrefix(rel, "..") {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write(b)
	}
}

// ---------- 工具函数 ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func countMarkdown(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			n++
		}
		return nil
	})
	return n
}

func countByExt(root, ext string, skipDirs map[string]bool) int {
	n := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ext) {
			n++
		}
		return nil
	})
	return n
}
