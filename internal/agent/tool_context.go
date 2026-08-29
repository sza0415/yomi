package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/ziangsun/szabot/internal/providers"
)

const (
	defaultToolResultMaxTokens = 1200
	defaultOutputReserveTokens = 1024
)

// ToolContextDecision records how a tool result was represented in the next
// model request. The original result remains available to Trace/Artifact.
type ToolContextDecision struct {
	Layer           string
	Action          string
	Stage           string
	Trigger         string
	AttemptedLayers []string
	SourceID        string
	Reason          string
	ArtifactID      string
	OriginalBytes   int
	ContextBytes    int
	TokensBefore    int
	TokensAfter     int
	Reversible      bool
	ArtifactError   string
}

func (r *Runner) prepareToolResult(ctx context.Context, conversation []providers.Message, call providers.ToolCall, result string) (string, *ToolContextDecision) {
	// StatusLine is a request-time injection and may have observable reads. Do
	// not query it a second time just for budget measurement; chatOnce measures
	// the final request including the injected line.
	currentMessages := conversation
	definitions := providerToolDefinitions(r.Tools)
	budget := r.effectiveContextBudget()
	before := budget.Evaluate(currentMessages, definitions)
	decision := &ToolContextDecision{
		Layer:           "none",
		Action:          "keep",
		Stage:           "tool_result",
		AttemptedLayers: []string{"budget_guard"},
		Reason:          "within tool result and context budget",
		OriginalBytes:   len(result),
		ContextBytes:    len(result),
		TokensBefore:    before.TotalTokens,
		Reversible:      true,
	}
	candidate := providers.Message{Role: providers.RoleTool, ToolCallID: call.ID, Content: result}
	after := budget.Evaluate(appendMessages(currentMessages, candidate), definitions)
	decision.TokensAfter = after.TotalTokens

	maxToolTokens := r.ToolResultMaxTokens
	if maxToolTokens <= 0 && (r.Artifacts != nil || r.MaxContextTokens > 0) {
		maxToolTokens = defaultToolResultMaxTokens
	}
	maxToolChars := maxToolTokens * 4
	tooLarge := maxToolChars > 0 && len([]rune(result)) > maxToolChars
	overBudget := after.Exceeded
	if tooLarge {
		decision.Trigger = "tool_result_oversized"
	} else if overBudget {
		decision.Trigger = "context_exceeded"
	}
	if !tooLarge && !overBudget {
		return result, decision
	}

	previewChars := maxToolChars
	if previewChars <= 0 {
		previewChars = 4800
	}
	if overBudget && after.AvailableMessageTokens > 0 {
		remaining := after.AvailableMessageTokens - before.MessageTokens
		if remaining > 0 && remaining*4 < previewChars {
			previewChars = remaining * 4
		}
	}
	if previewChars < 256 {
		previewChars = 256
	}
	preview := toolResultPreview(call.Name, result, previewChars)

	if r.Artifacts != nil {
		sessionID, _, ok := routeFrom(ctx)
		if !ok || strings.TrimSpace(sessionID) == "" {
			decision.ArtifactError = "session id is unavailable"
		} else {
			runID := ""
			if run, ok := runFrom(ctx); ok && run != nil {
				runID = run.ID
			}
			artifact, err := r.Artifacts.Put(sessionID, runID, call.Name, result, preview)
			if err == nil {
				decision.Layer = "artifact"
				decision.Action = "replace"
				decision.AttemptedLayers = append(decision.AttemptedLayers, "artifact")
				decision.SourceID = call.ID
				decision.ArtifactID = artifact.ID
				decision.Reason = "tool result exceeded the configured result or context budget"
				content := fmt.Sprintf("[artifact %s]\ntool: %s\nsize: %d bytes\n\n%s\n\nTo inspect the original result, use artifact_read with artifact_id=%s.", artifact.ID, call.Name, artifact.SizeBytes, preview, artifact.ID)
				decision.ContextBytes = len(content)
				decision.TokensAfter = budget.Evaluate(appendMessages(currentMessages, providers.Message{Role: providers.RoleTool, ToolCallID: call.ID, Content: content}), definitions).TotalTokens
				return content, decision
			}
			decision.ArtifactError = err.Error()
		}
	}

	decision.Layer = "budget_guard"
	decision.Action = "truncate"
	decision.AttemptedLayers = append(decision.AttemptedLayers, "budget_guard")
	decision.Reason = "artifact storage unavailable; bounded preview used"
	decision.ContextBytes = len(preview)
	decision.TokensAfter = budget.Evaluate(appendMessages(currentMessages, providers.Message{Role: providers.RoleTool, ToolCallID: call.ID, Content: preview}), definitions).TotalTokens
	return preview + "\n\n[tool result truncated]", decision
}

func appendMessages(messages []providers.Message, extra providers.Message) []providers.Message {
	result := make([]providers.Message, 0, len(messages)+1)
	result = append(result, messages...)
	result = append(result, extra)
	return result
}

func boundedToolPreview(content string, maxChars int) string {
	runes := []rune(content)
	if maxChars <= 0 || len(runes) <= maxChars {
		return content
	}
	if maxChars < 32 {
		return string(runes[:maxChars])
	}
	head := maxChars * 3 / 4
	tail := maxChars - head
	return string(runes[:head]) + "\n... [middle truncated] ...\n" + string(runes[len(runes)-tail:])
}

// toolResultPreview keeps the fields that make web results useful after
// compression. Generic tool output still uses the reversible head/tail view.
func toolResultPreview(toolName, content string, maxChars int) string {
	switch toolName {
	case "web_search":
		return webSearchPreview(content, maxChars)
	case "web_fetch":
		return webFetchPreview(content, maxChars)
	default:
		return boundedToolPreview(content, maxChars)
	}
}

func webSearchPreview(content string, maxChars int) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) == 0 || maxChars <= 0 {
		return boundedToolPreview(content, maxChars)
	}
	// Keep the search answer, result titles and URLs intact. Only snippets are
	// reduced when the complete set does not fit in the preview budget.
	var b strings.Builder
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "摘要：") || strings.HasPrefix(line, "Summary:") {
			b.WriteString(line)
			b.WriteString("\n\n")
			i++
			continue
		}
		if isSearchResultHeading(line) && i+1 < len(lines) {
			b.WriteString(line)
			b.WriteByte('\n')
			b.WriteString(strings.TrimSpace(lines[i+1]))
			if i+2 < len(lines) && strings.TrimSpace(lines[i+2]) != "" {
				b.WriteString("\n   ")
				b.WriteString(strings.TrimSpace(lines[i+2]))
			}
			b.WriteByte('\n')
			i += 3
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		i++
	}
	preview := strings.TrimSpace(b.String())
	if len([]rune(preview)) <= maxChars {
		return preview
	}
	// Preserve the result headers and URLs, then use a bounded body view.
	return boundedToolPreview(preview, maxChars)
}

func isSearchResultHeading(line string) bool {
	if len(line) < 3 || line[0] < '0' || line[0] > '9' {
		return false
	}
	for i := 1; i < len(line) && line[i] >= '0' && line[i] <= '9'; i++ {
		if i+1 < len(line) && line[i+1] == '.' {
			return true
		}
	}
	return false
}

func webFetchPreview(content string, maxChars int) string {
	rawLines := strings.Split(strings.TrimSpace(content), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) < 2 || maxChars <= 0 {
		return boundedToolPreview(content, maxChars)
	}
	// web_fetch already extracts article text; retain its title/source header
	// and compress only the body so the provenance survives truncation.
	header := lines[0] + "\n\n" + lines[1]
	body := strings.TrimSpace(strings.Join(lines[2:], "\n"))
	remaining := maxChars - len([]rune(header)) - 2
	if remaining <= 0 {
		return boundedToolPreview(header, maxChars)
	}
	return header + "\n\n" + boundedToolPreview(body, remaining)
}
