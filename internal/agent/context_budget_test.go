package agent

import (
	"testing"

	"github.com/ziangsun/szabot/internal/providers"
)

func TestContextBudgetAccountsForToolsAndOutputReserve(t *testing.T) {
	budget := ContextBudget{MaxContextTokens: 100, WarningRatio: 0.8, OutputReserveTokens: 20}
	messages := []providers.Message{{Role: providers.RoleUser, Content: "1234567890"}}
	definitions := []providers.ToolDefinition{{Name: "read_file", Description: "read a file", Parameters: []byte(`{"type":"object"}`)}}

	snapshot := budget.Evaluate(messages, definitions)
	if snapshot.MessageTokens != 3 {
		t.Fatalf("message tokens = %d, want 3", snapshot.MessageTokens)
	}
	if snapshot.ToolDefinitionTokens != 10 {
		t.Fatalf("tool definition tokens = %d, want 10", snapshot.ToolDefinitionTokens)
	}
	if snapshot.TotalTokens != 33 || snapshot.AvailableMessageTokens != 70 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Warning || snapshot.Exceeded {
		t.Fatalf("small request should be below warning: %#v", snapshot)
	}
}

func TestContextBudgetWarningAndExceeded(t *testing.T) {
	budget := ContextBudget{MaxContextTokens: 100, WarningRatio: 0.75, OutputReserveTokens: 10}
	warning := budget.Evaluate([]providers.Message{{Role: providers.RoleUser, Content: string(make([]byte, 300))}}, nil)
	if !warning.Warning || warning.Exceeded {
		t.Fatalf("expected warning only: %#v", warning)
	}
	exceeded := budget.Evaluate([]providers.Message{{Role: providers.RoleUser, Content: string(make([]byte, 401))}}, nil)
	if !exceeded.Exceeded {
		t.Fatalf("expected exceeded: %#v", exceeded)
	}
}
