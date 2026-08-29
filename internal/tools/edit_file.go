package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var editFileParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "相对于工作区根目录的文件路径"
		},
		"old_str": {
			"type": "string",
			"description": "要被替换的原文本。必须在文件中唯一出现（除非 replace_all 为 true），需包含足够上下文以保证唯一。"
		},
		"new_str": {
			"type": "string",
			"description": "替换后的新文本。传空字符串表示删除 old_str。"
		},
		"replace_all": {
			"type": "boolean",
			"description": "为 true 时替换所有匹配；默认 false，此时 old_str 必须唯一。"
		}
	},
	"required": ["path", "old_str", "new_str"],
	"additionalProperties": false
}`)

// EditFileTool performs an exact-string replacement inside one workspace file.
type EditFileTool struct {
	workspace string
	maxBytes  int64
}

// NewEditFile creates an edit tool scoped to workspace.
func NewEditFile(workspace string) (*EditFileTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("edit_file: %w", err)
	}
	return &EditFileTool{workspace: resolvedWorkspace, maxBytes: defaultReadFileMaxBytes}, nil
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Description() string {
	return "对已有文件做局部精确替换：把 old_str 替换为 new_str。默认要求 old_str 在文件中唯一，否则应补充更多上下文。适合代码的迭代修改。"
}

func (t *EditFileTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), editFileParameters...)
}

type editFileArguments struct {
	Path       string `json:"path"`
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all"`
}

// Execute replaces old_str with new_str. It rejects empty old_str, no match,
// and (unless replace_all) ambiguous matches, mirroring common coding-agent edit
// semantics so the model gets actionable feedback.
func (t *EditFileTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("edit_file: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("edit_file: arguments must be valid JSON")
	}

	var args editFileArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("edit_file: decode arguments: %w", err)
	}
	if args.OldStr == "" {
		return "", fmt.Errorf("edit_file: old_str must not be empty")
	}
	if args.OldStr == args.NewStr {
		return "", fmt.Errorf("edit_file: old_str and new_str are identical")
	}

	candidate, err := joinInWorkspace(t.workspace, args.Path)
	if err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("edit_file: resolve path: %w", err)
	}
	if !isWithinWorkspace(t.workspace, resolved) {
		return "", fmt.Errorf("edit_file: resolved path escapes the workspace")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("edit_file: stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("edit_file: path is a directory")
	}
	if info.Size() > t.maxBytes {
		return "", fmt.Errorf("edit_file: file exceeds %d bytes, too large to edit safely", t.maxBytes)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("edit_file: read file: %w", err)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("edit_file: file is not valid UTF-8 text")
	}
	original := string(data)

	count := strings.Count(original, args.OldStr)
	if count == 0 {
		return "", fmt.Errorf("edit_file: old_str not found in file")
	}
	if count > 1 && !args.ReplaceAll {
		return "", fmt.Errorf("edit_file: old_str appears %d times; add more context to make it unique or set replace_all", count)
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(original, args.OldStr, args.NewStr)
	} else {
		updated = strings.Replace(original, args.OldStr, args.NewStr, 1)
	}

	if err := os.WriteFile(resolved, []byte(updated), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("edit_file: write file: %w", err)
	}

	rel, _ := filepath.Rel(t.workspace, resolved)
	return fmt.Sprintf("已编辑 %s（替换 %d 处）", rel, count), nil
}
