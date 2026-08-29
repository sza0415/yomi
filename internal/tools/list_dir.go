package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultListDirMaxEntries = 200

// ignoredDirs are noise directories skipped during listing and searching.
var ignoredDirs = map[string]struct{}{
	".git":          {},
	"node_modules":  {},
	"__pycache__":   {},
	".venv":         {},
	"venv":          {},
	"dist":          {},
	"build":         {},
	".mypy_cache":   {},
	".pytest_cache": {},
	".ruff_cache":   {},
}

var listDirParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "相对于工作区根目录的目录路径。传空字符串或 \".\" 表示工作区根目录。"
		},
		"recursive": {
			"type": "boolean",
			"description": "为 true 时递归列出所有子目录；默认 false 只列一层。"
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`)

// ListDirTool lists directory contents inside one workspace. It is read-only.
type ListDirTool struct {
	workspace  string
	maxEntries int
}

// NewListDir creates a directory-listing tool scoped to workspace.
func NewListDir(workspace string) (*ListDirTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("list_dir: %w", err)
	}
	return &ListDirTool{workspace: resolvedWorkspace, maxEntries: defaultListDirMaxEntries}, nil
}

func (t *ListDirTool) Name() string { return "list_dir" }

func (t *ListDirTool) Description() string {
	return "列出工作区内某个目录的内容，帮助在路径未知时先探索结构。recursive=true 可递归列出。会自动忽略 .git、node_modules 等噪声目录。目录以 / 结尾。"
}

func (t *ListDirTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), listDirParameters...)
}

type listDirArguments struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

// Execute lists entries under path. Directories are suffixed with "/". Escaping
// the workspace is rejected via the shared sandbox helpers.
func (t *ListDirTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("list_dir: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("list_dir: arguments must be valid JSON")
	}

	var args listDirArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("list_dir: decode arguments: %w", err)
	}

	// Empty or "." means the workspace root.
	target := t.workspace
	rel := strings.TrimSpace(args.Path)
	if rel != "" && rel != "." {
		resolved, err := joinInWorkspace(t.workspace, rel)
		if err != nil {
			return "", fmt.Errorf("list_dir: %w", err)
		}
		target = resolved
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("list_dir: stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("list_dir: path is not a directory")
	}

	var entries []string
	total := 0

	if args.Recursive {
		walkErr := filepath.WalkDir(target, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if p == target {
				return nil
			}
			if d.IsDir() {
				if _, ignored := ignoredDirs[d.Name()]; ignored || strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
			}
			r, relErr := filepath.Rel(target, p)
			if relErr != nil {
				return nil
			}
			r = filepath.ToSlash(r)
			total++
			if len(entries) < t.maxEntries {
				if d.IsDir() {
					entries = append(entries, r+"/")
				} else {
					entries = append(entries, r)
				}
			}
			return nil
		})
		if walkErr != nil {
			return "", fmt.Errorf("list_dir: walk: %w", walkErr)
		}
	} else {
		dirEntries, err := os.ReadDir(target)
		if err != nil {
			return "", fmt.Errorf("list_dir: read directory: %w", err)
		}
		for _, d := range dirEntries {
			if _, ignored := ignoredDirs[d.Name()]; ignored {
				continue
			}
			total++
			if len(entries) < t.maxEntries {
				if d.IsDir() {
					entries = append(entries, d.Name()+"/")
				} else {
					entries = append(entries, d.Name())
				}
			}
		}
	}

	if total == 0 {
		return "（空目录）", nil
	}

	sort.Strings(entries)
	result := strings.Join(entries, "\n")
	if total > t.maxEntries {
		result += fmt.Sprintf("\n\n[结果已截断，共 %d 项，只显示前 %d 项。]", total, t.maxEntries)
	}
	return result, nil
}
