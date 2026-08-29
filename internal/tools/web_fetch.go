package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	defaultWebFetchMaxBytes = 2 << 20 // 抓取上限 2 MiB
	defaultWebFetchMaxRunes = 20000   // 返回给模型的正文字符上限
)

var webFetchParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"url": {
			"type": "string",
			"description": "要读取的网页 URL，必须是 http:// 或 https:// 开头的绝对地址。"
		}
	},
	"required": ["url"],
	"additionalProperties": false
}`)

// WebFetchTool 读取指定网页 URL 的正文内容。
//
// 用 golang.org/x/net/html 正规解析 DOM：跳过 script/style/noscript，
// 提取 <title>，在块级元素之间插入换行以保留段落结构，收集文本节点。
// 比手写正则更稳、更准确。
type WebFetchTool struct {
	httpClient *http.Client
	maxBytes   int64
	maxRunes   int
}

// NewWebFetch 创建网页读取工具。无需任何 API key。
func NewWebFetch() (*WebFetchTool, error) {
	return &WebFetchTool{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		maxBytes:   defaultWebFetchMaxBytes,
		maxRunes:   defaultWebFetchMaxRunes,
	}, nil
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "读取指定网页 URL 的具体内容，通常用于提取网页正文。传入 http/https 地址，返回去除 HTML 标签后的纯文本。"
}

func (t *WebFetchTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), webFetchParameters...)
}

type webFetchArguments struct {
	URL string `json:"url"`
}

// Execute 抓取网页并返回清洗后的纯文本（含标题）。
func (t *WebFetchTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.httpClient == nil {
		return "", fmt.Errorf("web_fetch: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("web_fetch: arguments must be valid JSON")
	}

	var args webFetchArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("web_fetch: decode arguments: %w", err)
	}

	target := strings.TrimSpace(args.URL)
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("web_fetch: url must be an absolute http:// or https:// address")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", fmt.Errorf("web_fetch: new request: %w", err)
	}
	// 带一个常见 UA，避免部分站点拒绝空 UA 的请求。
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; szabot/1.0; +https://github.com/ziangsun/szabot)")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("web_fetch: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("web_fetch: http %d", resp.StatusCode)
	}

	root, err := html.Parse(io.LimitReader(resp.Body, t.maxBytes))
	if err != nil {
		return "", fmt.Errorf("web_fetch: parse html: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	title := findTitle(root)
	text := extractText(root)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("web_fetch: page has no extractable text")
	}

	runes := []rune(text)
	truncated := false
	if len(runes) > t.maxRunes {
		text = string(runes[:t.maxRunes])
		truncated = true
	}

	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "标题：%s\n\n", title)
	}
	fmt.Fprintf(&b, "来源：%s\n\n", target)
	b.WriteString(text)
	if truncated {
		fmt.Fprintf(&b, "\n\n[正文已截断，最多返回 %d 个字符。]", t.maxRunes)
	}
	return b.String(), nil
}

// findTitle 深度优先查找第一个 <title> 的文本。
func findTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.DataAtom == atom.Title {
		return strings.TrimSpace(collectText(n))
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if title := findTitle(c); title != "" {
			return title
		}
	}
	return ""
}

// collectText 收集一个节点子树里的所有文本，仅用于 <title> 这类简单节点。
func collectText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		} else {
			b.WriteString(collectText(c))
		}
	}
	return b.String()
}

// 块级元素：遍历到它们时前后补换行，尽量保留段落结构。
var blockElements = map[atom.Atom]struct{}{
	atom.P: {}, atom.Div: {}, atom.Br: {}, atom.Li: {}, atom.Tr: {},
	atom.H1: {}, atom.H2: {}, atom.H3: {}, atom.H4: {}, atom.H5: {}, atom.H6: {},
	atom.Section: {}, atom.Article: {}, atom.Header: {}, atom.Footer: {},
	atom.Ul: {}, atom.Ol: {}, atom.Table: {}, atom.Blockquote: {}, atom.Pre: {},
}

// 需要整体跳过的元素（连同其子树）。
var skipElements = map[atom.Atom]struct{}{
	atom.Script: {}, atom.Style: {}, atom.Noscript: {}, atom.Head: {},
}

// extractText 遍历 DOM，跳过脚本/样式，块级元素间插换行，返回压缩空白后的纯文本。
func extractText(root *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			return
		case html.ElementNode:
			if _, skip := skipElements[n.DataAtom]; skip {
				return
			}
			_, isBlock := blockElements[n.DataAtom]
			if isBlock {
				b.WriteByte('\n')
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if isBlock {
				b.WriteByte('\n')
			}
			return
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
	}
	walk(root)
	return normalizeWhitespace(b.String())
}

// normalizeWhitespace 逐行 trim、合并行内多余空白、压缩连续空行。
func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
		if line == "" {
			blank++
			if blank <= 1 {
				out = append(out, "")
			}
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
