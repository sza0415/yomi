package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTavilyEndpoint   = "https://api.tavily.com/search"
	defaultWebSearchResults = 5
	maxWebSearchResults     = 10
)

var webSearchParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"query": {
			"type": "string",
			"description": "搜索关键词或问题。"
		},
		"max_results": {
			"type": "integer",
			"description": "可选。返回的结果条数，默认 5，最多 10。",
			"minimum": 1,
			"maximum": 10
		}
	},
	"required": ["query"],
	"additionalProperties": false
}`)

// WebSearchTool 通过 Tavily API 搜索互联网，返回清洗后的结果列表。
//
// 为什么选 Tavily：官方 JSON API、响应结构固定、专为 LLM 设计（直接给正文摘要），
// 比抓 Google/DuckDuckGo 的 HTML 稳定得多，且纯 net/http 实现、零第三方依赖。
type WebSearchTool struct {
	apiKey     string
	endpoint   string
	maxResults int
	httpClient *http.Client
}

// NewWebSearch 创建搜索工具。apiKey 为空时返回错误——由 main.go 决定跳过注册。
func NewWebSearch(apiKey string) (*WebSearchTool, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: TAVILY_API_KEY is empty")
	}
	return &WebSearchTool{
		apiKey:     apiKey,
		endpoint:   defaultTavilyEndpoint,
		maxResults: defaultWebSearchResults,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}, nil
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "搜索互联网，获取最新网页、新闻、资料或参考来源。返回若干条结果的标题、链接和摘要。"
}

func (t *WebSearchTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), webSearchParameters...)
}

type webSearchArguments struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// tavilyRequest / tavilyResponse 对应 Tavily /search 接口。
type tavilyRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results"`
	SearchDepth   string `json:"search_depth"`
	IncludeAnswer bool   `json:"include_answer"`
}

type tavilyResponse struct {
	Answer  string `json:"answer"`
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Execute 调用 Tavily /search 并把结果格式化成易读文本返回给模型。
func (t *WebSearchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.apiKey == "" {
		return "", fmt.Errorf("web_search: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("web_search: arguments must be valid JSON")
	}

	var args webSearchArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("web_search: decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", fmt.Errorf("web_search: query is required")
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = t.maxResults
	}
	if maxResults > maxWebSearchResults {
		maxResults = maxWebSearchResults
	}

	body, err := json.Marshal(tavilyRequest{
		APIKey:        t.apiKey,
		Query:         args.Query,
		MaxResults:    maxResults,
		SearchDepth:   "basic",
		IncludeAnswer: true,
	})
	if err != nil {
		return "", fmt.Errorf("web_search: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("web_search: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_search: do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("web_search: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("web_search: http %d: %s", resp.StatusCode, truncateText(string(respBody), 300))
	}

	var parsed tavilyResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("web_search: unmarshal response: %w", err)
	}

	return formatSearchResults(parsed), nil
}

func formatSearchResults(resp tavilyResponse) string {
	if len(resp.Results) == 0 {
		return "（未找到相关结果）"
	}

	var b strings.Builder
	if answer := strings.TrimSpace(resp.Answer); answer != "" {
		b.WriteString("摘要：")
		b.WriteString(answer)
		b.WriteString("\n\n")
	}
	for i, r := range resp.Results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1,
			strings.TrimSpace(r.Title),
			strings.TrimSpace(r.URL),
			truncateText(strings.TrimSpace(r.Content), 500))
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateText 按字符（rune）截断，避免把长正文糊进结果。
func truncateText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
