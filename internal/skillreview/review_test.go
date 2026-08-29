package skillreview

import (
	"bytes"
	"strings"
	"testing"
)

func TestEvaluateReportsPathAndNodeFailures(t *testing.T) {
	cases := []Case{{
		ID: "case-1", Name: "video", Expected: Expected{
			PathID: "path_video", Nodes: []string{"detect_input", "process_video"},
			ForbiddenNodes: []string{"process_article"}, OutputType: "video_result",
		},
	}}
	runs := []Run{{CaseID: "case-1", ActualPath: []string{"detect_input", "process_article"}, OutputType: "article_result"}}
	report := Evaluate(cases, runs, "test-sha")

	if report.Metrics.PassedCases != 0 || report.Metrics.FailedCases != 1 {
		t.Fatalf("metrics = %+v", report.Metrics)
	}
	if len(report.Results[0].Failures) != 4 {
		t.Fatalf("failures = %+v", report.Results[0].Failures)
	}
	if report.Results[0].Failures[0].Code != "path_mismatch" {
		t.Fatalf("first failure = %+v", report.Results[0].Failures[0])
	}
}

func TestWriteMarkdown(t *testing.T) {
	report := Evaluate([]Case{{ID: "case-1", Name: "ok", Expected: Expected{PathID: "path_ok", Nodes: []string{"path_ok"}}}}, []Run{{CaseID: "case-1", ActualPath: []string{"path_ok"}}}, "sha")
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "# Skill 评审报告") || !strings.Contains(buf.String(), "通过") {
		t.Fatalf("unexpected markdown:\n%s", buf.String())
	}
}

func TestCoverageCountsExpectedPathAndNodes(t *testing.T) {
	cases := []Case{{ID: "case-1", Expected: Expected{PathID: "path_video", Nodes: []string{"detect", "path_video"}}}}
	runs := []Run{{CaseID: "case-1", ActualPath: []string{"detect", "path_video", "extra"}}}
	report := Evaluate(cases, runs, "")
	if report.Metrics.PathCoverage != 1 || report.Metrics.NodeCoverage != 1 {
		t.Fatalf("metrics = %+v, want path/node coverage 1", report.Metrics)
	}
}

func TestMissingRunFailsCase(t *testing.T) {
	report := Evaluate([]Case{{ID: "case-1", Expected: Expected{PathID: "path_ok"}}}, nil, "")
	if report.Results[0].Passed || report.Results[0].Failures[0].Code != "missing_run" {
		t.Fatalf("result = %+v", report.Results[0])
	}
}

func TestEvaluateMultiSkillContract(t *testing.T) {
	cases := []Case{{
		ID: "system-case-1", Name: "多 Skill 内容分析",
		Expected: Expected{
			Skills: []SkillExpectation{{Name: "content-expert", Required: true}, {Name: "source-checker", Required: true}},
			ExecutionChain: []ChainStep{
				{ID: "select-content-expert", Skill: "content-expert", Required: true},
				{ID: "call-content-sim", Skill: "content-expert", Kind: NodeTool, Tool: "content-sim", Required: true},
				{ID: "verify-source", Skill: "source-checker", Kind: NodeValidation, Required: true},
			},
			Notices:    []string{"不得把模拟结果描述成真实联网结果"},
			OutputSpec: OutputExpectation{Type: "analysis_report", Contains: []string{"结论"}},
		},
	}}
	runs := []Run{{
		CaseID:         "system-case-1",
		SelectedSkills: []string{"content-expert", "source-checker"},
		ExecutionChain: []ChainStep{{ID: "select-content-expert"}, {ID: "call-content-sim"}, {ID: "verify-source"}},
		ToolCalls:      []string{"content-sim"}, AcknowledgedNotices: []string{"不得把模拟结果描述成真实联网结果"},
		OutputType: "analysis_report", OutputText: "结论：这是离线模拟结果。",
	}}
	report := Evaluate(cases, runs, "system-sha")
	if !report.Results[0].Passed {
		t.Fatalf("result = %+v", report.Results[0])
	}
	if report.Metrics.SkillSelectionRate != 1 || report.Metrics.ChainPassRate != 1 || report.Metrics.NoticePassRate != 1 || report.Metrics.OutputPassRate != 1 {
		t.Fatalf("metrics = %+v", report.Metrics)
	}
}
