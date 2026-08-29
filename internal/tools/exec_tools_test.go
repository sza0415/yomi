package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestListDirNonRecursiveAndEscape(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte(""), 0o644)
	os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	os.MkdirAll(filepath.Join(ws, ".git"), 0o755) // must be ignored

	tool, err := NewListDir(ws)
	if err != nil {
		t.Fatal(err)
	}

	out, err := tool.Execute(context.Background(), []byte(`{"path":"."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "sub/") {
		t.Fatalf("list out = %q, want a.txt and sub/", out)
	}
	if strings.Contains(out, ".git") {
		t.Fatalf("list out = %q, .git should be ignored", out)
	}

	if _, err := tool.Execute(context.Background(), []byte(`{"path":"../"}`)); err == nil ||
		!strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("escape error = %v, want workspace escape", err)
	}
}

func TestListDirRecursive(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(ws, "a", "b", "deep.txt"), []byte(""), 0o644)

	tool, _ := NewListDir(ws)
	out, err := tool.Execute(context.Background(), []byte(`{"path":".","recursive":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a/b/deep.txt") {
		t.Fatalf("recursive out = %q, want a/b/deep.txt", out)
	}
}

func TestNewSandboxRequiresDocker(t *testing.T) {
	// Config validation should reject empty image regardless of docker presence.
	_, err := NewSandbox(SandboxConfig{Workspace: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("err = %v, want image required", err)
	}
}

func TestProbeDocker(t *testing.T) {
	t.Run("daemon unavailable", func(t *testing.T) {
		dir := t.TempDir()
		binary := filepath.Join(dir, "docker")
		if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'Cannot connect to the Docker daemon' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := probeDocker(binary)
		if err == nil || !strings.Contains(err.Error(), "Docker daemon is not available") {
			t.Fatalf("probeDocker() error = %v, want daemon unavailable", err)
		}
	})

	t.Run("daemon available", func(t *testing.T) {
		dir := t.TempDir()
		binary := filepath.Join(dir, "docker")
		if err := os.WriteFile(binary, []byte("#!/bin/sh\n[ \"$1\" = info ]\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := probeDocker(binary); err != nil {
			t.Fatalf("probeDocker() error = %v, want nil", err)
		}
	})
}

// TestSandboxRunEcho actually runs a container. It is skipped when docker is
// not installed so CI without docker stays green.
func TestSandboxRunEcho(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; skipping sandbox integration test")
	}
	if err := probeDocker("docker"); err != nil {
		t.Skipf("docker daemon not available; skipping sandbox integration test: %v", err)
	}

	sandbox, err := NewSandbox(SandboxConfig{
		Image:     "busybox:latest",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := sandbox.Run(context.Background(), []string{"sh", "-s"}, "echo sandboxed")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(out, "sandboxed") {
		t.Fatalf("sandbox out = %q, want 'sandboxed'", out)
	}
}
