package tools

import (
	"context"
	"strings"
	"testing"
)

func TestArtifactStorePutReadAndSessionIsolation(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put("web:session-a", "run-1", "web_fetch", "0123456789", "0123...6789")
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID == "" || artifact.SizeBytes != 10 || artifact.SHA256 == "" {
		t.Fatalf("artifact = %#v", artifact)
	}

	got, err := store.Read(context.Background(), "web:session-a", artifact.ID, 2, 4)
	if err != nil || !strings.HasPrefix(got, "2345") {
		t.Fatalf("read = %q err=%v", got, err)
	}
	if _, err := store.Read(context.Background(), "web:session-b", artifact.ID, 0, 10); err == nil {
		t.Fatal("cross-session artifact read should fail")
	}
}

func TestArtifactReadToolUsesSessionFromContext(t *testing.T) {
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put("s", "run", "tool", "artifact body", "artifact preview")
	if err != nil {
		t.Fatal(err)
	}
	tool, err := NewArtifactReadTool(store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithSession(context.Background(), "s")
	got, err := tool.Execute(ctx, []byte(`{"artifact_id":"`+artifact.ID+`"}`))
	if err != nil || got != "artifact body" {
		t.Fatalf("artifact_read = %q err=%v", got, err)
	}
}
