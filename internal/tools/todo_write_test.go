package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runTodo 是调用 todo_write 的小助手，把 sessionID 注入 ctx 后执行。
func runTodo(t *testing.T, tool *TodoWriteTool, sessionID, args string) string {
	t.Helper()
	ctx := WithSession(context.Background(), sessionID)
	out, err := tool.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("todo_write execute error: %v", err)
	}
	return out
}

// TestStatusLineEmptyBeforeAnyWrite 验证：从未写入的会话，状态栏为空串，
// 以便 Runner 跳过注入、保持上下文前缀稳定。
func TestStatusLineEmptyBeforeAnyWrite(t *testing.T) {
	tool, err := NewTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	if line := tool.StatusLine("cli:local"); line != "" {
		t.Fatalf("StatusLine before any write = %q, want empty", line)
	}
	// 空 sessionID 归一到 default，同样应为空。
	if line := tool.StatusLine(""); line != "" {
		t.Fatalf("StatusLine(empty session) = %q, want empty", line)
	}
}

// TestStatusLineCountsCompleted 验证：状态栏首行给出 已完成/总数 概览，
// 且逐条渲染带状态标记。
func TestStatusLineCountsCompleted(t *testing.T) {
	tool, err := NewTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	runTodo(t, tool, "s1", `{"todos":[
		{"id":"1","content":"甲","status":"completed"},
		{"id":"2","content":"乙","status":"in_progress"},
		{"id":"3","content":"丙","status":"pending"}
	]}`)

	line := tool.StatusLine("s1")
	if !strings.HasPrefix(line, "任务清单 1/3 已完成") {
		t.Fatalf("StatusLine header = %q, want prefix 任务清单 1/3 已完成", line)
	}
	for _, want := range []string{"[x] 甲", "[~] 乙", "[ ] 丙"} {
		if !strings.Contains(line, want) {
			t.Fatalf("StatusLine %q missing %q", line, want)
		}
	}
}

// TestStatusLineReflectsMergeUpdate 验证：merge 更新某项状态后，
// 状态栏立刻反映新的进度（这正是"每轮重新读取"能看到最新态的基础）。
func TestStatusLineReflectsMergeUpdate(t *testing.T) {
	tool, err := NewTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	runTodo(t, tool, "s1", `{"todos":[
		{"id":"1","content":"甲","status":"in_progress"},
		{"id":"2","content":"乙","status":"pending"}
	]}`)
	if line := tool.StatusLine("s1"); !strings.HasPrefix(line, "任务清单 0/2 已完成") {
		t.Fatalf("before merge = %q, want 0/2", line)
	}

	// 只把第 1 项标记完成（merge 增量）。
	runTodo(t, tool, "s1", `{"merge":true,"todos":[{"id":"1","content":"甲","status":"completed"}]}`)
	if line := tool.StatusLine("s1"); !strings.HasPrefix(line, "任务清单 1/2 已完成") {
		t.Fatalf("after merge = %q, want 1/2", line)
	}
}

// TestStatusLineIsolatedPerSession 验证：不同会话的清单互不干扰。
func TestStatusLineIsolatedPerSession(t *testing.T) {
	tool, err := NewTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	runTodo(t, tool, "s1", `{"todos":[{"id":"1","content":"甲","status":"completed"}]}`)
	runTodo(t, tool, "s2", `{"todos":[{"id":"1","content":"X","status":"pending"}]}`)

	if line := tool.StatusLine("s1"); !strings.Contains(line, "甲") || strings.Contains(line, "X") {
		t.Fatalf("s1 status line = %q leaked across sessions", line)
	}
	if line := tool.StatusLine("s2"); !strings.Contains(line, "X") || strings.Contains(line, "甲") {
		t.Fatalf("s2 status line = %q leaked across sessions", line)
	}
}
