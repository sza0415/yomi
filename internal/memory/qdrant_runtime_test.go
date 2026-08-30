package memory

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureLocalQdrantRejectsRemoteEndpoint(t *testing.T) {
	err := EnsureLocalQdrant(context.Background(), "https://qdrant.example.com:6333")
	if err == nil || !strings.Contains(err.Error(), "non-local") {
		t.Fatalf("error = %v, want non-local endpoint error", err)
	}
}

func TestQdrantPointIDIsStableUUID(t *testing.T) {
	first := qdrantPointID("mem_123")
	second := qdrantPointID("mem_123")
	if first != second {
		t.Fatalf("point ID changed: %q != %q", first, second)
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("point ID = %q, want UUID shape", first)
	}
}
