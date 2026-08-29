package skillreview

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	runtimeTrace "github.com/ziangsun/szabot/internal/trace"
)

// TraceMapping describes how runtime events become observable Skill nodes.
// Runtime trace files remain unchanged; this is an evaluation-side projection.
type TraceMapping struct {
	Skill string             `json:"skill,omitempty"`
	Rules []TraceMappingRule `json:"rules"`
}

type TraceMappingRule struct {
	EventType string   `json:"event_type"`
	ToolName  string   `json:"tool_name,omitempty"`
	NodeID    string   `json:"node_id"`
	Kind      NodeKind `json:"kind"`
	Skill     string   `json:"skill,omitempty"`
	Condition string   `json:"condition,omitempty"`
	Source    string   `json:"source,omitempty"`
}

// LoadRuntimeTrace reads one raw runtime JSONL file.
func LoadRuntimeTrace(path string) ([]runtimeTrace.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open runtime trace: %w", err)
	}
	defer f.Close()
	var events []runtimeTrace.Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event runtimeTrace.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("parse runtime trace: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read runtime trace: %w", err)
	}
	return events, nil
}

// ProjectRuntimeTrace converts runtime events into the skill-review Run model.
func ProjectRuntimeTrace(events []runtimeTrace.Event, caseID string, mapping TraceMapping) Run {
	run := Run{CaseID: caseID, SkillVersion: "runtime-trace", ActualPath: []string{}}
	if len(events) > 0 {
		run.RunID = events[0].RunID
	}
	seenSkills := map[string]bool{}
	for _, event := range events {
		if event.Type == runtimeTrace.EventRunFinished {
			run.Status = event.Status
			if answer, ok := event.Data["answer"].(string); ok {
				run.OutputText = answer
			}
		}
		if event.Type == runtimeTrace.EventAssistantCompleted {
			if message, ok := event.Data["message"].(map[string]any); ok {
				if content, ok := message["content"].(string); ok {
					run.OutputText = content
				}
			}
		}
		for _, rule := range mapping.Rules {
			if !matchesRule(event, rule) {
				continue
			}
			kind := rule.Kind
			if kind == "" {
				kind = NodeTool
			}
			run.Trace = append(run.Trace, TraceEvent{NodeID: rule.NodeID, Kind: kind, Tool: rule.ToolName, Condition: rule.Condition, Source: firstNonEmpty(rule.Source, "runtime-trace"), RuntimeType: event.Type, Sequence: event.Sequence})
			if rule.NodeID != "" {
				run.ActualPath = append(run.ActualPath, rule.NodeID)
			}
			if rule.ToolName != "" && !contains(run.ToolCalls, rule.ToolName) {
				run.ToolCalls = append(run.ToolCalls, rule.ToolName)
			}
			if skill := firstNonEmpty(rule.Skill, mapping.Skill); skill != "" && !seenSkills[skill] {
				run.SelectedSkills = append(run.SelectedSkills, skill)
				seenSkills[skill] = true
			}
			break
		}
	}
	return run
}

func matchesRule(event runtimeTrace.Event, rule TraceMappingRule) bool {
	if rule.EventType == "" || event.Type != rule.EventType || rule.NodeID == "" {
		return false
	}
	if rule.ToolName == "" {
		return true
	}
	name, _ := event.Data["tool_name"].(string)
	return name == rule.ToolName
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
