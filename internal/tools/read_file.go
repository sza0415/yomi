package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const defaultReadFileMaxBytes int64 = 32 * 1024

var readFileParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "相对于工作区根目录的 UTF-8 文本文件路径"
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// ReadFileTool reads UTF-8 text files that resolve inside one workspace.
type ReadFileTool struct {
	workspace string
	maxBytes  int64
}

// NewReadFile creates a read-only file tool scoped to workspace.
func NewReadFile(workspace string) (*ReadFileTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}
	return &ReadFileTool{workspace: resolvedWorkspace, maxBytes: defaultReadFileMaxBytes}, nil
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "读取工作区内的 UTF-8 文本文件。路径必须相对于工作区根目录，不能读取工作区外的文件。"
}

func (t *ReadFileTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), readFileParameters...)
}

type readFileArguments struct {
	Path string `json:"path"`
}

// Execute reads at most 32 KiB and rejects absolute paths, traversal, and escaping symlinks.
func (t *ReadFileTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("read_file: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("read_file: arguments must be valid JSON")
	}

	var args readFileArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("read_file: decode arguments: %w", err)
	}

	candidate, err := joinInWorkspace(t.workspace, args.Path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("read_file: resolve path: %w", err)
	}
	if !isWithinWorkspace(t.workspace, resolved) {
		return "", fmt.Errorf("read_file: resolved path escapes the workspace")
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("read_file: stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file: path is a directory")
	}

	file, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("read_file: open file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, t.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read_file: read file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	truncated := int64(len(data)) > t.maxBytes
	if truncated {
		data = data[:t.maxBytes]
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("read_file: file is not valid UTF-8 text")
	}

	content := string(data)
	if truncated {
		content += "\n\n[文件内容已截断，最多返回 32768 字节。]"
	}
	return content, nil
}

func isWithinWorkspace(workspace, path string) bool {
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
