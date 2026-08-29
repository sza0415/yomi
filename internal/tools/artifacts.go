package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Artifact is the durable representation of a large tool result. The model
// receives a bounded preview while the complete content remains recoverable.
type Artifact struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	RunID       string    `json:"run_id,omitempty"`
	ToolName    string    `json:"tool_name"`
	SizeBytes   int       `json:"size_bytes"`
	SHA256      string    `json:"sha256"`
	Preview     string    `json:"preview"`
	CreatedAt   time.Time `json:"created_at"`
	ContentPath string    `json:"content_path"`
}

// ArtifactStore persists tool results below one session-scoped directory.
// Artifact IDs are generated locally and reads always require the session ID.
type ArtifactStore struct {
	dir string
	mu  sync.Mutex
}

// NewArtifactStore creates the artifact root with private permissions.
func NewArtifactStore(dir string) (*ArtifactStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("tools: artifact store dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tools: create artifact dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tools: secure artifact dir: %w", err)
	}
	return &ArtifactStore{dir: dir}, nil
}

// Put stores content and metadata atomically, returning the model-facing record.
func (s *ArtifactStore) Put(sessionID, runID, toolName, content, preview string) (Artifact, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return Artifact{}, fmt.Errorf("tools: artifact store is not initialized")
	}
	if strings.TrimSpace(sessionID) == "" {
		return Artifact{}, fmt.Errorf("tools: artifact session is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sessionDir := filepath.Join(s.dir, safeArtifactPart(sessionID))
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return Artifact{}, fmt.Errorf("tools: create artifact session dir: %w", err)
	}

	digest := sha256.Sum256([]byte(content))
	id := fmt.Sprintf("art_%d_%s", time.Now().UnixNano(), hex.EncodeToString(digest[:6]))
	contentName := id + ".content"
	contentPath := filepath.Join(sessionDir, contentName)
	metadataPath := filepath.Join(sessionDir, id+".json")

	if err := atomicWriteFile(contentPath, []byte(content), 0o600); err != nil {
		return Artifact{}, fmt.Errorf("tools: write artifact content: %w", err)
	}
	artifact := Artifact{
		ID:          id,
		SessionID:   sessionID,
		RunID:       runID,
		ToolName:    toolName,
		SizeBytes:   len(content),
		SHA256:      hex.EncodeToString(digest[:]),
		Preview:     preview,
		CreatedAt:   time.Now().UTC(),
		ContentPath: contentName,
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		return Artifact{}, fmt.Errorf("tools: encode artifact metadata: %w", err)
	}
	if err := atomicWriteFile(metadataPath, append(data, '\n'), 0o600); err != nil {
		return Artifact{}, fmt.Errorf("tools: write artifact metadata: %w", err)
	}
	return artifact, nil
}

// Read returns a bounded byte range from an artifact after validating its
// session ownership. maxBytes <= 0 uses the store default safety limit.
func (s *ArtifactStore) Read(ctx context.Context, sessionID, artifactID string, start, maxBytes int) (string, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return "", fmt.Errorf("tools: artifact store is not initialized")
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("tools: artifact session is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validArtifactID(artifactID) {
		return "", fmt.Errorf("tools: invalid artifact id")
	}
	if start < 0 {
		return "", fmt.Errorf("tools: artifact start must be non-negative")
	}
	if maxBytes <= 0 {
		maxBytes = 32 * 1024
	}
	if maxBytes > 256*1024 {
		maxBytes = 256 * 1024
	}

	sessionDir := filepath.Join(s.dir, safeArtifactPart(sessionID))
	metadataPath := filepath.Join(sessionDir, artifactID+".json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("tools: artifact %q not found", artifactID)
		}
		return "", fmt.Errorf("tools: read artifact metadata: %w", err)
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return "", fmt.Errorf("tools: parse artifact metadata: %w", err)
	}
	if artifact.SessionID != sessionID || artifact.ContentPath != artifact.ID+".content" {
		return "", fmt.Errorf("tools: artifact ownership validation failed")
	}

	file, err := os.Open(filepath.Join(sessionDir, artifact.ContentPath))
	if err != nil {
		return "", fmt.Errorf("tools: open artifact content: %w", err)
	}
	defer file.Close()
	if _, err := file.Seek(int64(start), 0); err != nil {
		return "", fmt.Errorf("tools: seek artifact content: %w", err)
	}
	buf := make([]byte, maxBytes)
	n, readErr := file.Read(buf)
	if readErr != nil && n == 0 {
		return "", fmt.Errorf("tools: read artifact content: %w", readErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result := string(buf[:n])
	if start+n < artifact.SizeBytes {
		result += fmt.Sprintf("\n\n[artifact content truncated; start=%d, returned=%d, total=%d bytes]", start, n, artifact.SizeBytes)
	}
	return result, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func safeArtifactPart(value string) string {
	safe := filepath.Base(filepath.Clean("/" + value))
	if safe == "." || safe == "/" || safe == "" {
		return "default"
	}
	return safe
}

func validArtifactID(value string) bool {
	return strings.HasPrefix(value, "art_") && filepath.Base(value) == value && !strings.Contains(value, "..")
}

var artifactReadParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"artifact_id": {"type": "string", "description": "要读取的 Artifact ID"},
		"start": {"type": "integer", "minimum": 0, "description": "从字节偏移开始读取，默认 0"},
		"max_bytes": {"type": "integer", "minimum": 1, "maximum": 262144, "description": "最多读取的字节数，默认 32768"}
	},
	"required": ["artifact_id"],
	"additionalProperties": false
}`)

// ArtifactReadTool reads a bounded range from a session-owned artifact.
type ArtifactReadTool struct {
	store *ArtifactStore
}

// NewArtifactReadTool creates the read capability for a given artifact store.
func NewArtifactReadTool(store *ArtifactStore) (*ArtifactReadTool, error) {
	if store == nil {
		return nil, fmt.Errorf("artifact_read: store is nil")
	}
	return &ArtifactReadTool{store: store}, nil
}

func (t *ArtifactReadTool) Name() string { return "artifact_read" }

func (t *ArtifactReadTool) Description() string {
	return "按 Artifact ID 和字节范围读取此前外置的大型工具结果。只能读取当前会话自己的 Artifact。"
}

func (t *ArtifactReadTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), artifactReadParameters...)
}

func (t *ArtifactReadTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if t == nil || t.store == nil {
		return "", fmt.Errorf("artifact_read: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("artifact_read: arguments must be valid JSON")
	}
	var args struct {
		ArtifactID string `json:"artifact_id"`
		Start      int    `json:"start"`
		MaxBytes   int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("artifact_read: decode arguments: %w", err)
	}
	return t.store.Read(ctx, sessionFrom(ctx), args.ArtifactID, args.Start, args.MaxBytes)
}
