package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

var writeFileParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "相对于工作区根目录的文件路径，父目录不存在时会自动创建"
		},
		"content": {
			"type": "string",
			"description": "要写入的完整 UTF-8 文本内容，会完全覆盖已有文件"
		}
	},
	"required": ["path", "content"],
	"additionalProperties": false
}`)

// WriteFileTool creates or fully overwrites a UTF-8 text file inside one workspace.
type WriteFileTool struct {
	workspace string
}

// NewWriteFile creates a write tool scoped to workspace.
func NewWriteFile(workspace string) (*WriteFileTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	return &WriteFileTool{workspace: resolvedWorkspace}, nil
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "创建新文件或完全重写已有文件。路径必须相对于工作区根目录，不能写到工作区外。父目录会自动创建。"
}

func (t *WriteFileTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), writeFileParameters...)
}

type writeFileArguments struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Execute writes content atomically-ish (parent dirs created) and refuses to
// clobber a directory or escape the workspace.
func (t *WriteFileTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("write_file: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("write_file: arguments must be valid JSON")
	}

	var args writeFileArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("write_file: decode arguments: %w", err)
	}
	if !utf8.ValidString(args.Content) {
		return "", fmt.Errorf("write_file: content is not valid UTF-8 text")
	}

	candidate, err := joinInWorkspace(t.workspace, args.Path)
	if err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}

	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return "", fmt.Errorf("write_file: path is a directory")
	}

	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		return "", fmt.Errorf("write_file: create parent directories: %w", err)
	}
	if err := os.WriteFile(candidate, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: write file: %w", err)
	}

	rel, _ := filepath.Rel(t.workspace, candidate)
	return fmt.Sprintf("已写入 %s（%d 字节）", rel, len(args.Content)), nil
}
