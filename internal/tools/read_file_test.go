package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileToolReadsWorkspaceFileAndRejectsEscapes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello tools"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool, err := NewReadFile(workspace)
	if err != nil {
		t.Fatal(err)
	}

	content, err := tool.Execute(context.Background(), []byte(`{"path":"note.txt"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if content != "hello tools" {
		t.Fatalf("Execute() content = %q, want %q", content, "hello tools")
	}

	_, err = tool.Execute(context.Background(), []byte(`{"path":"../outside.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("Execute() traversal error = %v, want workspace escape error", err)
	}
}

func TestRegistryRejectsUnknownTool(t *testing.T) {
	registry := NewRegistry()
	_, err := registry.Execute(context.Background(), "missing", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Execute() error = %v, want unknown-tool error", err)
	}
}
