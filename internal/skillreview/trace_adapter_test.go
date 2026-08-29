package skillreview

import (
	"testing"
	"time"

	runtimeTrace "github.com/ziangsun/szabot/internal/trace"
)

func TestProjectRuntimeTrace(t *testing.T) {
	events := []runtimeTrace.Event{
		{RunID: "r1", Type: runtimeTrace.EventRunStarted, Sequence: 1, Timestamp: time.Now(), Data: map[string]any{}},
		{RunID: "r1", Type: runtimeTrace.EventToolExecutionStarted, Sequence: 2, Timestamp: time.Now(), Data: map[string]any{"tool_name": "read_file"}},
		{RunID: "r1", Type: runtimeTrace.EventRunFinished, Sequence: 3, Timestamp: time.Now(), Status: "completed", Data: map[string]any{"answer": "done"}},
	}
	run := ProjectRuntimeTrace(events, "case-1", TraceMapping{Skill: "kbcli", Rules: []TraceMappingRule{{EventType: runtimeTrace.EventToolExecutionStarted, ToolName: "read_file", NodeID: "read-skill", Kind: NodeTool}}})
	if run.RunID != "r1" || run.OutputText != "done" || len(run.ActualPath) != 1 || run.ActualPath[0] != "read-skill" {
		t.Fatalf("run = %#v", run)
	}
	if len(run.SelectedSkills) != 1 || run.SelectedSkills[0] != "kbcli" || run.Trace[0].RuntimeType == "" {
		t.Fatalf("projection = %#v", run)
	}
}
