// Package tools defines the local capabilities that an agent may expose to a model.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Tool describes one capability that can be requested by the model.
// Parameters must be an object-shaped JSON Schema.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, arguments json.RawMessage) (string, error)
}

// RetryClassifier is optional. Tools with idempotent, transient operations may
// implement it to opt into Runner-level retries.
type RetryClassifier interface {
	Retryable(error) bool
}

// Definition is the provider-neutral schema sent to the model.
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Registry is the explicit allowlist of tools available to an agent.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the allowlist. Duplicate or invalid names are rejected.
func (r *Registry) Register(tool Tool) error {
	if r == nil {
		return fmt.Errorf("tools: registry is nil")
	}
	if tool == nil {
		return fmt.Errorf("tools: cannot register a nil tool")
	}

	name := strings.TrimSpace(tool.Name())
	if name == "" {
		return fmt.Errorf("tools: cannot register a tool with an empty name")
	}
	if !json.Valid(tool.Parameters()) {
		return fmt.Errorf("tools: tool %q has invalid parameter schema", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tools: tool %q is already registered", name)
	}
	r.tools[name] = tool
	return nil
}

// Definitions returns a stable, alphabetically ordered snapshot of registered tools.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	definitions := make([]Definition, 0, len(names))
	for _, name := range names {
		tool := r.tools[name]
		parameters := append(json.RawMessage(nil), tool.Parameters()...)
		definitions = append(definitions, Definition{
			Name:        name,
			Description: tool.Description(),
			Parameters:  parameters,
		})
	}
	return definitions
}

// Execute runs a registered tool. Unknown tools are rejected rather than executed.
func (r *Registry) Execute(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	if r == nil {
		return "", fmt.Errorf("tools: registry is nil")
	}

	r.mu.RLock()
	tool, ok := r.tools[name]
	names := make([]string, 0, len(r.tools))
	if !ok {
		for registered := range r.tools {
			names = append(names, registered)
		}
	}
	r.mu.RUnlock()

	if !ok {
		sort.Strings(names)
		return "", fmt.Errorf("tools: tool %q is not registered (available: %s)", name, strings.Join(names, ", "))
	}
	return tool.Execute(ctx, arguments)
}

// Lookup returns a registered tool for callers that need optional capabilities.
func (r *Registry) Lookup(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}
