package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const defaultGlobMaxResults = 200

var globParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "相对于工作区根目录的 glob 模式，支持 ** 递归匹配，例如 **/*.go 或 internal/*.go"
		}
	},
	"required": ["pattern"],
	"additionalProperties": false
}`)

// GlobTool finds files by name pattern within one workspace.
type GlobTool struct {
	workspace  string
	maxResults int
}

// NewGlob creates a filename-search tool scoped to workspace.
func NewGlob(workspace string) (*GlobTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}
	return &GlobTool{workspace: resolvedWorkspace, maxResults: defaultGlobMaxResults}, nil
}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "按文件名模式查找工作区内的文件，支持 ** 递归通配（如 **/*.py 找出所有 Python 文件）。返回相对路径列表。"
}

func (t *GlobTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), globParameters...)
}

type globArguments struct {
	Pattern string `json:"pattern"`
}

// Execute walks the workspace and returns slash-separated relative paths whose
// path matches the pattern. Hidden dirs like .git are skipped.
func (t *GlobTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("glob: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("glob: arguments must be valid JSON")
	}

	var args globArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("glob: decode arguments: %w", err)
	}
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		return "", fmt.Errorf("glob: pattern is required")
	}
	pattern = filepath.ToSlash(pattern)

	var matches []string
	walkErr := filepath.WalkDir(t.workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		rel, relErr := filepath.Rel(t.workspace, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}

		if matchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("glob: walk workspace: %w", walkErr)
	}

	if len(matches) == 0 {
		return "（无匹配文件）", nil
	}

	sort.Strings(matches)
	truncated := false
	if len(matches) > t.maxResults {
		matches = matches[:t.maxResults]
		truncated = true
	}

	result := strings.Join(matches, "\n")
	if truncated {
		result += fmt.Sprintf("\n\n[结果已截断，最多返回 %d 条。]", t.maxResults)
	}
	return result, nil
}

// matchGlob supports "**" (match across directory separators) on top of the
// standard path.Match semantics. Without "**", it also allows a bare pattern
// like "*.go" to match by basename for convenience.
func matchGlob(pattern, name string) bool {
	if strings.Contains(pattern, "**") {
		return matchDoubleStar(pattern, name)
	}
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	// Convenience: "*.go" matches any file's basename.
	if !strings.Contains(pattern, "/") {
		if ok, _ := path.Match(pattern, path.Base(name)); ok {
			return true
		}
	}
	return false
}

// matchDoubleStar splits on "**" and matches the pattern segments greedily.
// Handles common cases like "**/*.go", "internal/**/*.go", "**/testdata/**".
func matchDoubleStar(pattern, name string) bool {
	parts := strings.Split(pattern, "**")
	return matchParts(parts, name)
}

func matchParts(parts []string, name string) bool {
	if len(parts) == 1 {
		ok, _ := path.Match(parts[0], name)
		return ok
	}

	head := strings.TrimSuffix(parts[0], "/")
	tail := strings.TrimPrefix(parts[len(parts)-1], "/")

	// Leading "**": match the tail pattern against name or any suffix segment.
	if head == "" {
		return matchTail(tail, name)
	}

	// Require name to start under the head directory.
	if name != head && !strings.HasPrefix(name, head+"/") {
		return false
	}
	rest := strings.TrimPrefix(name, head)
	rest = strings.TrimPrefix(rest, "/")
	return matchTail(tail, rest)
}

// matchTail matches pattern against name where name may have leading dir
// segments consumed by a preceding "**".
func matchTail(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	if ok, _ := path.Match(pattern, name); ok {
		return true
	}
	// Try matching pattern against each trailing sub-path so "**/*.go" works.
	segments := strings.Split(name, "/")
	for i := range segments {
		sub := strings.Join(segments[i:], "/")
		if ok, _ := path.Match(pattern, sub); ok {
			return true
		}
		if ok, _ := path.Match(pattern, segments[i]); ok && i == len(segments)-1 {
			return true
		}
	}
	return false
}
