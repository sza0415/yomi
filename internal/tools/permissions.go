package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type PermissionMode string

const (
	PermissionSafe           PermissionMode = "safe"
	PermissionWorkspaceWrite PermissionMode = "workspace-write"
	PermissionFull           PermissionMode = "full"
)

type PermissionRequest struct {
	Tool      string
	Arguments json.RawMessage
	Reason    string
}

type PermissionGate interface {
	Check(context.Context, PermissionRequest) error
}

// PolicyGate is a host-side approval gate. It never trusts model output as an
// approval; approval must arrive through the injected Asker channel.
//
// Tools approved with "Allow always" are remembered for the lifetime of this
// gate (normally the process). The state is intentionally in-memory so a
// restart returns to the conservative default policy.
type PolicyGate struct {
	Mode PermissionMode

	mu            sync.RWMutex
	allowedAlways map[string]struct{}
}

func NewPolicyGate(mode PermissionMode) *PolicyGate {
	return &PolicyGate{Mode: mode, allowedAlways: make(map[string]struct{})}
}

func (g *PolicyGate) Check(ctx context.Context, req PermissionRequest) error {
	if g == nil {
		return nil
	}
	if req.Tool == "ask_user_question" || isReadOnlyTool(req.Tool) {
		return nil
	}
	if g.isAlwaysAllowed(req.Tool) {
		return nil
	}
	if g.Mode == PermissionFull {
		return nil
	}
	if g.Mode == PermissionWorkspaceWrite {
		switch req.Tool {
		case "write_file", "edit_file", "todo_write":
			return nil
		}
	}
	asker, ok := AskerFromContext(ctx)
	if !ok {
		return fmt.Errorf("permission denied: %s requires user approval but no interactive channel is available", req.Tool)
	}
	question := fmt.Sprintf("Allow tool %q? %s", req.Tool, strings.TrimSpace(req.Reason))
	answer, err := asker.Ask(ctx, question, []string{"Allow once", "Allow always", "Deny"})
	if err != nil {
		return fmt.Errorf("permission approval: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "allow always", "always allow", "总是允许", "始终允许", "2":
		g.allowAlways(req.Tool)
		return nil
	case "allow once", "allow", "允许一次", "1":
		return nil
	case "deny", "拒绝", "3":
		return fmt.Errorf("permission denied by user: %s", req.Tool)
	default:
		return fmt.Errorf("permission denied by user: %s", req.Tool)
	}
}

func (g *PolicyGate) isAlwaysAllowed(tool string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.allowedAlways[tool]
	return ok
}

func (g *PolicyGate) allowAlways(tool string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.allowedAlways == nil {
		g.allowedAlways = make(map[string]struct{})
	}
	g.allowedAlways[tool] = struct{}{}
}

func isReadOnlyTool(name string) bool {
	switch name {
	case "read_file", "list_dir", "glob", "grep":
		return true
	default:
		return false
	}
}
