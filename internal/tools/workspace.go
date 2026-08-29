package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveWorkspace validates that workspace exists, is a directory, and returns
// its symlink-resolved absolute path. All file tools share this so the sandbox
// boundary is defined in exactly one place.
func resolveWorkspace(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", fmt.Errorf("workspace is empty")
	}

	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", resolved)
	}
	return resolved, nil
}

// joinInWorkspace resolves a workspace-relative path and guarantees the result
// stays inside the workspace. It rejects absolute paths and ".." traversal
// without requiring the target to exist yet (so it is safe for writes).
func joinInWorkspace(workspace, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the workspace")
	}

	candidate := filepath.Clean(filepath.Join(workspace, rel))
	if !isWithinWorkspace(workspace, candidate) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return candidate, nil
}
