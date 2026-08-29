package agent

import (
	"strings"
	"testing"
)

func TestWebSearchPreviewKeepsSources(t *testing.T) {
	content := "摘要：北京高校概览\n\n1. 清华大学\n   https://example.com/tsinghua\n   清华大学是一所综合性研究型大学。\n2. 北京大学\n   https://example.com/pku\n   北京大学位于北京。"
	preview := webSearchPreview(content, 220)
	if !strings.Contains(preview, "https://example.com/tsinghua") {
		t.Fatalf("preview lost first source: %q", preview)
	}
	if !strings.Contains(preview, "https://example.com/pku") {
		t.Fatalf("preview lost second source: %q", preview)
	}
}

func TestWebFetchPreviewKeepsProvenance(t *testing.T) {
	content := "标题：北京高校\n\n来源：https://example.com/page\n\n正文内容很长，包含需要进一步阅读的事实。"
	preview := webFetchPreview(content, 120)
	if !strings.Contains(preview, "标题：北京高校") || !strings.Contains(preview, "来源：https://example.com/page") {
		t.Fatalf("preview lost provenance: %q", preview)
	}
}
