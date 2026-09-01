package configstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdatePersistsCompleteSnapshotIncludingEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".yomi", "config.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(map[string]string{"STALE": "remove-me", "SZABOT_PROVIDER": "echo", "SZABOT_WEB": ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(map[string]string{"SZABOT_PROVIDER": "echo", "SZABOT_WEB": ""}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if text == "" || strings.Contains(text, "STALE") || !strings.Contains(text, `"SZABOT_PROVIDER": "echo"`) || !strings.Contains(text, `"SZABOT_WEB": ""`) {
		t.Fatalf("config snapshot = %s", text)
	}
}
