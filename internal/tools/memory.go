package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/memory"
)

const memoryToolLimit = 50

var memoryBrowseParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"level": {
			"type": "string",
			"enum": ["kinds", "subjects", "attributes", "memories"],
			"description": "要查看的层级。按 kinds -> subjects -> attributes -> memories 逐层浏览。"
		},
		"kind": {
			"type": "string",
			"enum": ["fact", "preference", "episode"],
			"description": "查看 subjects、attributes 或 memories 时必填。"
		},
		"subject": {
			"type": "string",
			"description": "查看 attributes 或 memories 时必填。subject 是记忆主体，通常为 self，也可能是朋友、家人或其他实体。"
		},
		"attribute": {
			"type": "string",
			"description": "查看 memories 时必填。attribute 只是模型生成的导航标签，语义相近的标签可能需要一并检查。"
		},
		"include_conflicts": {
			"type": "boolean",
			"description": "是否包含待解决的冲突记忆，默认 false。"
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"maximum": 50,
			"description": "最多返回的目录项或记忆数，默认 50。"
		}
	},
	"required": ["level"],
	"additionalProperties": false
}`)

type MemoryBrowseTool struct {
	store memory.Browser
}

func NewMemoryBrowse(store memory.Browser) (*MemoryBrowseTool, error) {
	if store == nil {
		return nil, errors.New("memory_browse: store is nil")
	}
	return &MemoryBrowseTool{store: store}, nil
}

func (t *MemoryBrowseTool) Name() string { return "memory_browse" }

func (t *MemoryBrowseTool) Description() string {
	return "分层浏览当前用户的长期记忆。kind 只有 fact、preference、episode；先查看某个 kind 下的 subjects，再查看 subject 下的 attributes，最后读取 attribute 下的记忆。user_id 由系统绑定。"
}

func (t *MemoryBrowseTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), memoryBrowseParameters...)
}

type memoryBrowseArguments struct {
	Level            memory.BrowseLevel `json:"level"`
	Kind             *string            `json:"kind"`
	Subject          *string            `json:"subject"`
	Attribute        *string            `json:"attribute"`
	IncludeConflicts bool               `json:"include_conflicts"`
	Limit            int                `json:"limit"`
}

func (t *MemoryBrowseTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	userID := strings.TrimSpace(userFrom(ctx))
	if userID == "" {
		return "", errors.New("memory_browse: current user is unavailable")
	}
	var args memoryBrowseArguments
	if err := decodeToolArguments(raw, &args); err != nil {
		return "", fmt.Errorf("memory_browse: %w", err)
	}
	query := memory.BrowseQuery{UserID: userID, Level: args.Level, IncludeConflicts: args.IncludeConflicts, Limit: boundedMemoryLimit(args.Limit)}
	switch args.Level {
	case memory.BrowseKinds:
	case memory.BrowseSubjects:
		if args.Kind == nil {
			return "", errors.New("memory_browse: kind is required for subjects")
		}
		query.Kind = *args.Kind
	case memory.BrowseAttributes:
		if args.Kind == nil || args.Subject == nil {
			return "", errors.New("memory_browse: kind and subject are required for attributes")
		}
		query.Kind, query.Subject = *args.Kind, *args.Subject
	case memory.BrowseMemories:
		if args.Kind == nil || args.Subject == nil || args.Attribute == nil {
			return "", errors.New("memory_browse: kind, subject, and attribute are required for memories")
		}
		query.Kind, query.Subject, query.Attribute = *args.Kind, *args.Subject, *args.Attribute
	default:
		return "", fmt.Errorf("memory_browse: unsupported level %q", args.Level)
	}
	result, err := t.store.Browse(ctx, query)
	if err != nil {
		return "", err
	}
	return marshalMemoryToolResult(memoryBrowseView(result))
}

var memorySearchParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "用自然语言描述要找的记忆。搜索会同时使用关键词和可用的语义索引，不要求 attribute 精确匹配。"
		},
		"kind": {
			"type": "string",
			"enum": ["fact", "preference", "episode"],
			"description": "可选的记忆类型过滤。"
		},
		"include_conflicts": {
			"type": "boolean",
			"description": "是否包含待解决的冲突记忆，默认 false。"
		},
		"limit": {
			"type": "integer",
			"minimum": 1,
			"maximum": 50,
			"description": "最多返回的记忆数，默认 8。"
		}
	},
	"required": ["query"],
	"additionalProperties": false
}`)

type MemorySearchTool struct {
	store memory.Store
}

func NewMemorySearch(store memory.Store) (*MemorySearchTool, error) {
	if store == nil {
		return nil, errors.New("memory_search: store is nil")
	}
	return &MemorySearchTool{store: store}, nil
}

func (t *MemorySearchTool) Name() string { return "memory_search" }

func (t *MemorySearchTool) Description() string {
	return "按自然语言搜索当前用户的长期记忆，不依赖 subject 或 attribute 精确相等。适合不知道记忆位于哪个层级时使用；user_id 由系统绑定。"
}

func (t *MemorySearchTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), memorySearchParameters...)
}

type memorySearchArguments struct {
	Query            string `json:"query"`
	Kind             string `json:"kind"`
	IncludeConflicts bool   `json:"include_conflicts"`
	Limit            int    `json:"limit"`
}

func (t *MemorySearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	userID := strings.TrimSpace(userFrom(ctx))
	if userID == "" {
		return "", errors.New("memory_search: current user is unavailable")
	}
	var args memorySearchArguments
	if err := decodeToolArguments(raw, &args); err != nil {
		return "", fmt.Errorf("memory_search: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", errors.New("memory_search: query is required")
	}
	query := memory.Query{UserID: userID, Text: args.Query, Limit: boundedMemorySearchLimit(args.Limit), IncludeConflicts: args.IncludeConflicts}
	if strings.TrimSpace(args.Kind) != "" {
		if !validMemoryKind(args.Kind) {
			return "", fmt.Errorf("memory_search: unsupported kind %q", args.Kind)
		}
		query.Kinds = []string{args.Kind}
	}
	items, err := t.store.Search(ctx, query)
	if err != nil {
		return "", err
	}
	return marshalMemoryToolResult(map[string]any{"memories": memorySummaries(items)})
}

var memoryGetParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"ids": {
			"type": "array",
			"items": {"type": "string"},
			"minItems": 1,
			"maxItems": 20,
			"description": "memory_browse 或 memory_search 返回的记忆 ID。"
		}
	},
	"required": ["ids"],
	"additionalProperties": false
}`)

type MemoryGetTool struct {
	store memory.Store
}

func NewMemoryGet(store memory.Store) (*MemoryGetTool, error) {
	if store == nil {
		return nil, errors.New("memory_get: store is nil")
	}
	return &MemoryGetTool{store: store}, nil
}

func (t *MemoryGetTool) Name() string { return "memory_get" }

func (t *MemoryGetTool) Description() string {
	return "按 ID 读取当前用户记忆的完整内容、证据、状态和时间信息。只能读取 memory_browse 或 memory_search 发现的当前用户记忆。"
}

func (t *MemoryGetTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), memoryGetParameters...)
}

func (t *MemoryGetTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	userID := strings.TrimSpace(userFrom(ctx))
	if userID == "" {
		return "", errors.New("memory_get: current user is unavailable")
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := decodeToolArguments(raw, &args); err != nil {
		return "", fmt.Errorf("memory_get: %w", err)
	}
	if len(args.IDs) == 0 || len(args.IDs) > 20 {
		return "", errors.New("memory_get: ids must contain 1 to 20 values")
	}
	items := make([]memory.Memory, 0, len(args.IDs))
	seen := make(map[string]struct{}, len(args.IDs))
	for _, id := range args.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return "", errors.New("memory_get: ids must not contain empty values")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		item, err := t.store.Get(ctx, userID, id)
		if err != nil {
			return "", err
		}
		if item.Status != memory.StatusActive && item.Status != memory.StatusConflict {
			return "", fmt.Errorf("memory_get: memory %s is not recallable", id)
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(time.Now().UTC()) {
			return "", fmt.Errorf("memory_get: memory %s has expired", id)
		}
		items = append(items, item)
	}
	return marshalMemoryToolResult(map[string]any{"memories": items})
}

func decodeToolArguments(raw json.RawMessage, target any) error {
	if !json.Valid(raw) {
		return errors.New("arguments must be valid JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("arguments must contain exactly one JSON value")
		}
		return fmt.Errorf("decode trailing arguments: %w", err)
	}
	return nil
}

func boundedMemoryLimit(limit int) int {
	if limit <= 0 || limit > memoryToolLimit {
		return memoryToolLimit
	}
	return limit
}

func boundedMemorySearchLimit(limit int) int {
	if limit <= 0 {
		return 8
	}
	if limit > memoryToolLimit {
		return memoryToolLimit
	}
	return limit
}

func validMemoryKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case memory.KindFact, memory.KindPreference, memory.KindEpisode:
		return true
	default:
		return false
	}
}

func marshalMemoryToolResult(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("memory tool: encode result: %w", err)
	}
	return string(data), nil
}

func memoryBrowseView(result memory.BrowseResult) map[string]any {
	view := map[string]any{"level": result.Level}
	if len(result.Entries) > 0 {
		view["entries"] = result.Entries
	}
	if len(result.Memories) > 0 {
		view["memories"] = memorySummaries(result.Memories)
	}
	return view
}

func memorySummaries(items []memory.Memory) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{
			"id": item.ID, "kind": item.Kind, "subject": item.Subject,
			"attribute": item.Attribute, "value": item.Value,
			"content": item.Content, "status": item.Status,
			"confidence": item.Confidence, "updated_at": item.UpdatedAt,
		})
	}
	return result
}
