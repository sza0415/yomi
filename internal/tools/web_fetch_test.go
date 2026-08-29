package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchExtractsText(t *testing.T) {
	const page = `<!DOCTYPE html>
<html>
<head>
	<title>示例标题</title>
	<style>.a{color:red}</style>
	<script>var x = 1;</script>
</head>
<body>
	<h1>大标题</h1>
	<p>第一段正文。</p>
	<p>第二段正文。</p>
	<script>console.log("should be dropped")</script>
</body>
</html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	tool, err := NewWebFetch()
	if err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(webFetchArguments{URL: srv.URL})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !strings.Contains(out, "标题：示例标题") {
		t.Errorf("output missing title, got:\n%s", out)
	}
	for _, want := range []string{"大标题", "第一段正文。", "第二段正文。"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
	// script/style 内容必须被剔除。
	for _, dropped := range []string{"var x = 1", "color:red", "should be dropped"} {
		if strings.Contains(out, dropped) {
			t.Errorf("output should not contain %q, got:\n%s", dropped, out)
		}
	}
}

func TestWebFetchRejectsNonHTTPURL(t *testing.T) {
	tool, err := NewWebFetch()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "ftp://example.com", "file:///etc/passwd", "not-a-url"} {
		args, _ := json.Marshal(webFetchArguments{URL: bad})
		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Errorf("Execute(%q) expected error, got nil", bad)
		}
	}
}
