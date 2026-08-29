package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	content := `---
name: github
description: "用 gh 操作 GitHub"
metadata: {"nanobot":{"requires":{"bins":["gh"],"env":["GH_TOKEN"]},"always":true}}
---
# 正文
一些说明。`

	meta, ok := parseFrontmatter(content)
	if !ok {
		t.Fatal("expected frontmatter to parse")
	}
	if meta.Name != "github" {
		t.Errorf("name = %q, want github", meta.Name)
	}
	if meta.Description != "用 gh 操作 GitHub" {
		t.Errorf("description = %q", meta.Description)
	}
	if len(meta.Requires.Bins) != 1 || meta.Requires.Bins[0] != "gh" {
		t.Errorf("requires.bins = %v", meta.Requires.Bins)
	}
	if len(meta.Requires.Env) != 1 || meta.Requires.Env[0] != "GH_TOKEN" {
		t.Errorf("requires.env = %v", meta.Requires.Env)
	}
	if !meta.Always {
		t.Error("always = false, want true")
	}
}

func TestStripFrontmatter(t *testing.T) {
	content := "---\nname: x\n---\n# Title\nbody"
	body := stripFrontmatter(content)
	if strings.Contains(body, "name: x") {
		t.Errorf("frontmatter not stripped: %q", body)
	}
	if !strings.HasPrefix(body, "# Title") {
		t.Errorf("body = %q, want to start with # Title", body)
	}
}

func TestSummaryUsesRelativePathAndOverride(t *testing.T) {
	ws := t.TempDir()
	// workspace 技能（优先级最高）。
	writeSkill(t, filepath.Join(ws, "skills"), "weather",
		"---\nname: weather\ndescription: \"查天气\"\n---\n# W")
	// 同名技能放到内置目录，应被 workspace 覆盖。
	builtin := t.TempDir()
	writeSkill(t, builtin, "weather",
		"---\nname: weather\ndescription: \"内置版本-不应出现\"\n---\n# W")
	// 仅存在于内置目录的技能。
	writeSkill(t, builtin, "notes",
		"---\nname: notes\ndescription: \"记笔记\"\n---\n# N")

	loader := NewLoader(ws, WithBuiltinDir(builtin))
	summary := loader.Summary()

	if !strings.Contains(summary, "`skills/weather/SKILL.md`") {
		t.Errorf("summary missing workspace relative path:\n%s", summary)
	}
	if strings.Contains(summary, "内置版本-不应出现") {
		t.Errorf("workspace skill should override builtin:\n%s", summary)
	}
	if !strings.Contains(summary, "**notes**") {
		t.Errorf("builtin-only skill missing:\n%s", summary)
	}
}

func TestSummaryMarksUnavailable(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "skills"), "needsbin",
		"---\nname: needsbin\ndescription: \"需要外部命令\"\nmetadata: {\"requires\":{\"bins\":[\"definitely-not-a-real-binary-xyz\"]}}\n---\n# body")

	summary := NewLoader(ws).Summary()
	if !strings.Contains(summary, "(unavailable:") {
		t.Errorf("expected unavailable marker:\n%s", summary)
	}
}

func TestAlwaysBodyNotInSummary(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "skills"), "core",
		"---\nname: core\ndescription: \"常驻\"\nalways: true\n---\n# Core body")

	loader := NewLoader(ws)
	if strings.Contains(loader.Summary(), "**core**") {
		t.Error("always skill should not appear in L1 summary")
	}
	if !strings.Contains(loader.AlwaysBodies(), "Core body") {
		t.Error("always skill body should be loaded")
	}
}

func TestEnabledFiltersSkills(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, "skills"), "github", "---\nname: github\ndescription: \"GitHub\"\n---\n# GitHub body")
	writeSkill(t, filepath.Join(ws, "skills"), "kbcli", "---\nname: kbcli\ndescription: \"影库\"\n---\n# KB body")
	loader := NewLoader(ws, WithEnabled("kbcli"))
	if strings.Contains(loader.Summary(), "github") || !strings.Contains(loader.Summary(), "kbcli") {
		t.Fatalf("filtered summary = %q", loader.Summary())
	}
}
