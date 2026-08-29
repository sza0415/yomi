package agent

import (
	"errors"

	"github.com/ziangsun/szabot/internal/providers"
)

const defaultContextWarningRatio = 0.8

// ErrContextBudgetExceeded means a request cannot fit after accounting for
// tool definitions and the output reservation.
var ErrContextBudgetExceeded = errors.New("agent: context budget exceeded")

// ContextBudget is the shared budget policy used by ContextManager and Runner.
// It measures the same inputs at the cross-run and in-run boundaries.
type ContextBudget struct {
	MaxContextTokens    int
	WarningRatio        float64
	OutputReserveTokens int
}

// BudgetSnapshot describes one context measurement. MessageTokens excludes
// tool definitions and the output reservation; TotalTokens includes both.
type BudgetSnapshot struct {
	MessageTokens          int  `json:"message_tokens"`
	ToolDefinitionTokens   int  `json:"tool_definition_tokens"`
	OutputReserveTokens    int  `json:"output_reserve_tokens"`
	TotalTokens            int  `json:"total_tokens"`
	MaxContextTokens       int  `json:"max_context_tokens"`
	WarningTokens          int  `json:"warning_tokens"`
	AvailableMessageTokens int  `json:"available_message_tokens"`
	Warning                bool `json:"warning"`
	Exceeded               bool `json:"exceeded"`
}

// Evaluate measures messages plus the tool definitions sent with the request.
// A zero MaxContextTokens means unlimited, but still returns useful counts.
func (b ContextBudget) Evaluate(messages []providers.Message, definitions []providers.ToolDefinition) BudgetSnapshot {
	ratio := b.WarningRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = defaultContextWarningRatio
	}
	messageTokens := estimateMessagesTokens(messages)
	toolTokens := estimateToolDefinitionsTokens(definitions)
	total := messageTokens + toolTokens + maxInt(0, b.OutputReserveTokens)
	warningTokens := 0
	if b.MaxContextTokens > 0 {
		warningTokens = int(float64(b.MaxContextTokens) * ratio)
	}
	available := 0
	if b.MaxContextTokens > 0 {
		available = b.MaxContextTokens - toolTokens - maxInt(0, b.OutputReserveTokens)
		if available < 0 {
			available = 0
		}
	}
	return BudgetSnapshot{
		MessageTokens:          messageTokens,
		ToolDefinitionTokens:   toolTokens,
		OutputReserveTokens:    maxInt(0, b.OutputReserveTokens),
		TotalTokens:            total,
		MaxContextTokens:       b.MaxContextTokens,
		WarningTokens:          warningTokens,
		AvailableMessageTokens: available,
		Warning:                b.MaxContextTokens > 0 && total >= warningTokens,
		Exceeded:               b.MaxContextTokens > 0 && total > b.MaxContextTokens,
	}
}

func estimateToolDefinitionsTokens(definitions []providers.ToolDefinition) int {
	n := 0
	for _, definition := range definitions {
		n += len(definition.Name) + len(definition.Description) + len(definition.Parameters)
	}
	return (n + 3) / 4
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
