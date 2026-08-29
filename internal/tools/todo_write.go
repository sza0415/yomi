package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

var todoWriteParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"merge": {
			"type": "boolean",
			"description": "true 时按 id 合并/更新已有清单；false 时用本次 todos 完全替换整张清单。默认 false。"
		},
		"todos": {
			"type": "array",
			"description": "任务项数组。",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "任务唯一标识。"},
					"content": {"type": "string", "description": "任务描述。"},
					"status": {
						"type": "string",
						"description": "任务状态。",
						"enum": ["pending", "in_progress", "completed", "cancelled"]
					}
				},
				"required": ["id", "content", "status"],
				"additionalProperties": false
			}
		}
	},
	"required": ["todos"],
	"additionalProperties": false
}`)

// 合法状态集合。
var validTodoStatus = map[string]struct{}{
	"pending":     {},
	"in_progress": {},
	"completed":   {},
	"cancelled":   {},
}

// todoItem 是一条任务。
type todoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoWriteTool 创建和更新任务清单，记录 Agent 当前的计划、进度和完成状态。
//
// 状态按 SessionID 分开保存在进程内存里：不同会话互不干扰，进程重启即清空
// （与 M8 之前"暂无持久化"的整体现状一致）。
type TodoWriteTool struct {
	mu    sync.Mutex
	byID  map[string][]todoItem // sessionID -> 有序任务列表
	order map[string][]string   // sessionID -> id 出现顺序
}

// NewTodoWrite 创建任务清单工具。
func NewTodoWrite() (*TodoWriteTool, error) {
	return &TodoWriteTool{
		byID:  make(map[string][]todoItem),
		order: make(map[string][]string),
	}, nil
}

func (t *TodoWriteTool) Name() string { return "todo_write" }

func (t *TodoWriteTool) Description() string {
	return "创建和更新任务清单，记录 Agent 当前的计划、进度和完成状态。merge=false 覆盖整张清单，merge=true 按 id 合并更新。返回渲染后的清单。"
}

func (t *TodoWriteTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), todoWriteParameters...)
}

type todoWriteArguments struct {
	Merge bool       `json:"merge"`
	Todos []todoItem `json:"todos"`
}

// Execute 按 merge 语义更新当前会话的清单，并返回渲染后的完整清单。
func (t *TodoWriteTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("todo_write: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("todo_write: arguments must be valid JSON")
	}

	var args todoWriteArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("todo_write: decode arguments: %w", err)
	}
	if len(args.Todos) == 0 {
		return "", fmt.Errorf("todo_write: todos must contain at least one item")
	}
	for i, item := range args.Todos {
		if strings.TrimSpace(item.ID) == "" {
			return "", fmt.Errorf("todo_write: todos[%d].id is required", i)
		}
		if strings.TrimSpace(item.Content) == "" {
			return "", fmt.Errorf("todo_write: todos[%d].content is required", i)
		}
		if _, ok := validTodoStatus[item.Status]; !ok {
			return "", fmt.Errorf("todo_write: todos[%d].status %q is invalid", i, item.Status)
		}
	}

	session := sessionFrom(ctx)
	if session == "" {
		session = "default"
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if !args.Merge {
		// 完全替换。
		list := make([]todoItem, len(args.Todos))
		copy(list, args.Todos)
		orderIDs := make([]string, 0, len(args.Todos))
		index := make(map[string]int, len(args.Todos))
		deduped := list[:0]
		for _, item := range list {
			if pos, seen := index[item.ID]; seen {
				deduped[pos] = item // 后者覆盖前者
				continue
			}
			index[item.ID] = len(deduped)
			deduped = append(deduped, item)
			orderIDs = append(orderIDs, item.ID)
		}
		t.byID[session] = append([]todoItem(nil), deduped...)
		t.order[session] = orderIDs
		return renderTodos(t.byID[session]), nil
	}

	// 合并：按 id 更新已有项，新 id 追加到末尾。
	existing := t.byID[session]
	orderIDs := t.order[session]
	index := make(map[string]int, len(existing))
	for i, item := range existing {
		index[item.ID] = i
	}
	for _, item := range args.Todos {
		if pos, ok := index[item.ID]; ok {
			existing[pos] = item
			continue
		}
		index[item.ID] = len(existing)
		existing = append(existing, item)
		orderIDs = append(orderIDs, item.ID)
	}
	t.byID[session] = existing
	t.order[session] = orderIDs
	return renderTodos(existing), nil
}

// StatusLine 返回某个会话当前任务清单的「Agent 状态栏」文本。
//
// 它与工具返回值（renderTodos）共用同一套渲染，但多带一行进度概览
// （如「任务清单 2/5 已完成」），供 Runner 在每轮调用 LLM 前作为一条
// user 消息注入到上下文末尾——从而让模型每一轮都感知自己的计划与进度，
// 抑制无限循环、状态遗忘与目标偏离。
//
// 清单为空（该会话从未调用过 todo_write）时返回空串，调用方据此跳过注入，
// 保持无状态会话的上下文前缀稳定、对 KV Cache 友好。
func (t *TodoWriteTool) StatusLine(sessionID string) string {
	if t == nil {
		return ""
	}
	if sessionID == "" {
		sessionID = "default"
	}

	t.mu.Lock()
	items := append([]todoItem(nil), t.byID[sessionID]...)
	t.mu.Unlock()

	if len(items) == 0 {
		return ""
	}
	return renderTodoStatus(items)
}

// renderTodoStatus 渲染带进度概览头的状态栏文本（供 StatusLine 使用）。
func renderTodoStatus(items []todoItem) string {
	done := 0
	for _, item := range items {
		if item.Status == "completed" {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "任务清单 %d/%d 已完成\n", done, len(items))
	for _, item := range items {
		fmt.Fprintf(&b, "%s %s\n", statusMark(item.Status), item.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTodos 把清单渲染成带状态标记的文本。
func renderTodos(items []todoItem) string {
	if len(items) == 0 {
		return "（清单为空）"
	}
	var b strings.Builder
	b.WriteString("任务清单：\n")
	for _, item := range items {
		fmt.Fprintf(&b, "%s %s\n", statusMark(item.Status), item.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

func statusMark(status string) string {
	switch status {
	case "completed":
		return "[x]"
	case "in_progress":
		return "[~]"
	case "cancelled":
		return "[-]"
	default: // pending
		return "[ ]"
	}
}
