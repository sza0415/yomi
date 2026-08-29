package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultGrepMaxMatches   = 200
	defaultGrepMaxFileBytes = 1 << 20 // 1 MiB per file
)

var grepParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"pattern": {
			"type": "string",
			"description": "要搜索的正则表达式（Go RE2 语法）"
		},
		"include": {
			"type": "string",
			"description": "可选。只在匹配该 glob 的文件里搜索，例如 **/*.go。省略则搜索全部文本文件。"
		}
	},
	"required": ["pattern"],
	"additionalProperties": false
}`)

// GrepTool searches file contents for a regular expression within one workspace.
type GrepTool struct {
	workspace    string
	maxMatches   int
	maxFileBytes int64
}

// NewGrep creates a content-search tool scoped to workspace.
func NewGrep(workspace string) (*GrepTool, error) {
	resolvedWorkspace, err := resolveWorkspace(workspace)
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	return &GrepTool{
		workspace:    resolvedWorkspace,
		maxMatches:   defaultGrepMaxMatches,
		maxFileBytes: defaultGrepMaxFileBytes,
	}, nil
}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "在工作区文件内容中按正则表达式搜索文本，返回匹配的 文件:行号:内容。可用 include 限定文件（如 **/*.go）。"
}

func (t *GrepTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), grepParameters...)
}

type grepArguments struct {
	Pattern string `json:"pattern"`
	Include string `json:"include"`
}

// Execute walks the workspace, filters by the optional include glob, and reports
// every matching line as "relpath:lineno:content". Binary/oversized files are skipped.
func (t *GrepTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.workspace == "" {
		return "", fmt.Errorf("grep: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("grep: arguments must be valid JSON")
	}

	var args grepArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("grep: decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return "", fmt.Errorf("grep: pattern is required")
	}

	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid regular expression: %w", err)
	}
	include := filepath.ToSlash(strings.TrimSpace(args.Include))

	var lines []string
	truncated := false

	walkErr := filepath.WalkDir(t.workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
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
		if include != "" && !matchGlob(include, rel) {
			return nil
		}
		if len(lines) >= t.maxMatches {
			truncated = true
			return filepath.SkipAll
		}

		fileLines, stop := t.searchFile(p, rel, re, t.maxMatches-len(lines))
		lines = append(lines, fileLines...)
		if stop {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("grep: walk workspace: %w", walkErr)
	}

	if len(lines) == 0 {
		return "（无匹配内容）", nil
	}

	sort.Strings(lines)
	result := strings.Join(lines, "\n")
	if truncated {
		result += fmt.Sprintf("\n\n[结果已截断，最多返回 %d 条匹配。]", t.maxMatches)
	}
	return result, nil
}

// searchFile scans one file line by line. It returns matched "rel:line:content"
// entries (capped by budget) and whether the match budget was hit.
func (t *GrepTool) searchFile(absPath, rel string, re *regexp.Regexp, budget int) ([]string, bool) {
	info, err := os.Stat(absPath)
	if err != nil || info.Size() > t.maxFileBytes {
		return nil, false
	}

	file, err := os.Open(absPath)
	if err != nil {
		return nil, false
	}
	defer file.Close()

	var out []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		text := scanner.Text()
		if !utf8.ValidString(text) {
			return out, false // treat as binary; stop scanning this file
		}
		if re.MatchString(text) {
			out = append(out, fmt.Sprintf("%s:%d:%s", rel, lineNo, text))
			if len(out) >= budget {
				return out, true
			}
		}
	}
	_ = scanner.Err() // a scan error just ends this file's search; other files continue
	return out, false
}
