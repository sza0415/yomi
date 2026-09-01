package configstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store persists Yomi's optional environment-style settings in a small JSON file.
// Process environment variables remain the highest-precedence override.
type Store struct {
	Path string
	mu   sync.RWMutex
	Data map[string]string
}

func Load(path string) (*Store, error) {
	s := &Store{Path: path, Data: map[string]string{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.Data); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) ApplyToEnv() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, value := range s.Data {
		if strings.TrimSpace(key) != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

func (s *Store) Snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.Data))
	for k, v := range s.Data {
		out[k] = v
	}
	return out
}

func (s *Store) Update(values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" {
			continue
		}
		// Keep empty values in the snapshot so the file records every setting,
		// including an explicitly disabled option.
		next[key] = value
	}
	s.Data = next
	b, err := json.MarshalIndent(s.Data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	return nil
}
