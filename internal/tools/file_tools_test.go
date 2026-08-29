package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

func TestWriteFileCreatesFileAndRejectsEscape(t *testing.T) {
	ws := t.TempDir()
	tool, err := NewWriteFile(ws)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Execute(context.Background(), mustJSON(t, `{"path":"sub/a.txt","content":"hi"}`)); err != nil {
		t.Fatalf("write error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(ws, "sub", "a.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("file content = %q err = %v", got, err)
	}

	if _, err := tool.Execute(context.Background(), mustJSON(t, `{"path":"../evil.txt","content":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("escape error = %v, want workspace escape", err)
	}
}

func TestEditFileUniqueAndAmbiguous(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "code.go"), []byte("foo\nbar\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, err := NewEditFile(ws)
	if err != nil {
		t.Fatal(err)
	}

	// Ambiguous "foo" without replace_all must fail.
	if _, err := tool.Execute(context.Background(), mustJSON(t, `{"path":"code.go","old_str":"foo","new_str":"baz"}`)); err == nil ||
		!strings.Contains(err.Error(), "appears 2 times") {
		t.Fatalf("ambiguous error = %v, want appears 2 times", err)
	}

	// replace_all rewrites both.
	if _, err := tool.Execute(context.Background(), mustJSON(t, `{"path":"code.go","old_str":"foo","new_str":"baz","replace_all":true}`)); err != nil {
		t.Fatalf("replace_all error = %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "code.go"))
	if string(got) != "baz\nbar\nbaz\n" {
		t.Fatalf("content = %q", got)
	}

	// Not found.
	if _, err := tool.Execute(context.Background(), mustJSON(t, `{"path":"code.go","old_str":"nope","new_str":"x"}`)); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("not-found error = %v", err)
	}
}

func TestGlobRecursive(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(ws, "root.py"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(ws, "a", "b", "deep.py"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(ws, "a", "note.txt"), []byte(""), 0o644)

	tool, err := NewGlob(ws)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), mustJSON(t, `{"pattern":"**/*.py"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "root.py") || !strings.Contains(out, "a/b/deep.py") {
		t.Fatalf("glob out = %q, want both .py files", out)
	}
	if strings.Contains(out, "note.txt") {
		t.Fatalf("glob out = %q, should not include .txt", out)
	}
}

func TestGrepMatchesWithInclude(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc doThing() {}\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "readme.md"), []byte("doThing docs\n"), 0o644)

	tool, err := NewGrep(ws)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), mustJSON(t, `{"pattern":"doThing","include":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go:2:") {
		t.Fatalf("grep out = %q, want main.go line 2", out)
	}
	if strings.Contains(out, "readme.md") {
		t.Fatalf("grep out = %q, include should exclude .md", out)
	}
}
