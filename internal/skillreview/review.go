// Package skillreview 提供单个 Skill 的路径评审和回归报告能力。
package skillreview

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Case 描述一条 Skill 评审用例。
type Case struct {
	ID       string   `json:"case_id"`
	Name     string   `json:"name"`
	Input    any      `json:"input,omitempty"`
	Expected Expected `json:"expected"`
	Tags     []string `json:"tags,omitempty"`
}

// NodeKind 表示 Skill Path 中节点的语义类型。
type NodeKind string

const (
	NodeInput      NodeKind = "input"
	NodeDecision   NodeKind = "decision"
	NodeValidation NodeKind = "validation"
	NodeTool       NodeKind = "tool"
	NodeOutput     NodeKind = "output"
	NodeFallback   NodeKind = "fallback"
)

// NodeDefinition 是 Path 中可观察的节点。Tool、Condition 和 Notes 用于
// 描述节点为什么存在以及执行时需要注意什么，而不只是记录一个节点名称。
//
// 分叉表达：一个 Skill 常常"按条件走不同分支、不同终点有不同预期"。
//   - 线性节点：用 Next 指向唯一后继（0 或 1 个）；不填时按数组顺序隐式相连；
//   - 判断节点（kind=decision）：用 Branches 列出多个互斥分支，每个分支带进入
//     条件 When、指向的后继节点 To，以及该分支终点对应的预期 Expect。
//
// 两个字段都可省略，因此旧的线性 Path 完全兼容。
type NodeDefinition struct {
	ID             string   `json:"id"`
	Kind           NodeKind `json:"kind"`
	Tool           string   `json:"tool,omitempty"`
	Condition      string   `json:"condition,omitempty"`
	ExpectedBranch string   `json:"expected_branch,omitempty"`
	Required       bool     `json:"required"`
	Notes          []string `json:"notes,omitempty"`
	// Next 是线性后继节点 id（分叉时用 Branches 而非 Next）。
	Next []string `json:"next,omitempty"`
	// Branches 是判断节点的分支出口，每个分支走向不同后继与预期。
	Branches []Branch `json:"branches,omitempty"`
}

// Branch 是判断节点（decision）的一条分支：满足 When 时走向 To 节点，
// 该分支最终应产生的结果由 Expect 描述（可选，作为该分叉的预期草稿）。
type Branch struct {
	When   string             `json:"when"`             // 进入该分支的条件
	To     string             `json:"to"`               // 指向的后继节点 id
	Label  string             `json:"label,omitempty"`  // 分支简称（展示用）
	Expect *OutputExpectation `json:"expect,omitempty"` // 该分支终点对应的预期结果
}

// PathDefinition 是从 Skill 中抽取出来的一条完整执行路径。
type PathDefinition struct {
	PathID          string           `json:"path_id"`
	Name            string           `json:"name"`
	EntryConditions []string         `json:"entry_conditions,omitempty"`
	Nodes           []NodeDefinition `json:"nodes"`
	Exit            string           `json:"exit,omitempty"`
	Exceptions      []string         `json:"exceptions,omitempty"`
}

// SkillExpectation 描述本次输入应该命中的 Skill。
type SkillExpectation struct {
	Name     string   `json:"name"`
	Required bool     `json:"required"`
	Notes    []string `json:"notes,omitempty"`
}

// ChainStep 描述跨 Skill 的完整执行链路中的一个步骤。
type ChainStep struct {
	ID       string   `json:"id"`
	Skill    string   `json:"skill,omitempty"`
	Kind     NodeKind `json:"kind,omitempty"`
	Tool     string   `json:"tool,omitempty"`
	Required bool     `json:"required"`
	Notes    []string `json:"notes,omitempty"`
}

// OutputExpectation 描述最终输出的可验证条件。
type OutputExpectation struct {
	Type       string   `json:"type,omitempty"`
	Contains   []string `json:"contains,omitempty"`
	NotContain []string `json:"not_contains,omitempty"`
}

// Expected 是用例对执行路径和结果的期望。
type Expected struct {
	PathID          string             `json:"path_id"`
	Nodes           []string           `json:"nodes"`
	NodeSpecs       []NodeDefinition   `json:"node_specs,omitempty"`
	Skills          []SkillExpectation `json:"expected_skills,omitempty"`
	ExecutionChain  []ChainStep        `json:"execution_chain,omitempty"`
	Notices         []string           `json:"notices,omitempty"`
	OutputSpec      OutputExpectation  `json:"output_spec,omitempty"`
	ForbiddenSkills []string           `json:"forbidden_skills,omitempty"`
	ForbiddenNodes  []string           `json:"forbidden_nodes,omitempty"`
	OutputType      string             `json:"output_type,omitempty"`
	ExpectedOutput  any                `json:"output,omitempty"`
	Constraints     []string           `json:"constraints,omitempty"`
}

// Run 是一次实际执行的结构化 Trace。
type Run struct {
	CaseID              string       `json:"case_id"`
	RunID               string       `json:"run_id,omitempty"`
	SkillVersion        string       `json:"skill_version,omitempty"`
	Status              string       `json:"status,omitempty"`
	SelectedSkills      []string     `json:"selected_skills,omitempty"`
	ActualPath          []string     `json:"actual_path"`
	Trace               []TraceEvent `json:"trace,omitempty"`
	ExecutionChain      []ChainStep  `json:"execution_chain,omitempty"`
	ToolCalls           []string     `json:"tool_calls,omitempty"`
	AcknowledgedNotices []string     `json:"acknowledged_notices,omitempty"`
	OutputType          string       `json:"output_type,omitempty"`
	Output              any          `json:"output,omitempty"`
	OutputText          string       `json:"output_text,omitempty"`
}

// TraceEvent 是 Agent 执行过程中产生的结构化节点事件。
type TraceEvent struct {
	NodeID      string   `json:"node_id"`
	Kind        NodeKind `json:"kind"`
	Tool        string   `json:"tool,omitempty"`
	Condition   string   `json:"condition,omitempty"`
	Decision    string   `json:"decision,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	Source      string   `json:"source,omitempty"`
	RuntimeType string   `json:"runtime_type,omitempty"`
	Sequence    uint64   `json:"sequence,omitempty"`
}

// Failure 描述一个用例未通过的具体原因。
type Failure struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	FailedNode   string `json:"failed_node,omitempty"`
	ExpectedPath string `json:"expected_path,omitempty"`
	ActualPath   string `json:"actual_path,omitempty"`
}

// CaseResult 是单条用例的评审结果。
type CaseResult struct {
	CaseID   string    `json:"case_id"`
	Name     string    `json:"name"`
	Passed   bool      `json:"passed"`
	Expected Expected  `json:"expected"`
	Actual   *Run      `json:"actual,omitempty"`
	Failures []Failure `json:"failures,omitempty"`
}

// Metrics 是评审摘要指标。
type Metrics struct {
	TotalCases         int     `json:"total_cases"`
	PassedCases        int     `json:"passed_cases"`
	FailedCases        int     `json:"failed_cases"`
	PathCoverage       float64 `json:"path_coverage"`
	NodeCoverage       float64 `json:"node_coverage"`
	PathMatchRate      float64 `json:"path_match_rate"`
	NodePassRate       float64 `json:"node_pass_rate"`
	OutputPassRate     float64 `json:"output_pass_rate"`
	SkillSelectionRate float64 `json:"skill_selection_rate"`
	ChainPassRate      float64 `json:"chain_pass_rate"`
	NoticePassRate     float64 `json:"notice_pass_rate"`
}

// Report 是一次完整评审的结果。
type Report struct {
	SkillVersion string       `json:"skill_version,omitempty"`
	Metrics      Metrics      `json:"metrics"`
	Results      []CaseResult `json:"results"`
}

// LoadCases 从 JSON 文件读取测试用例。文件内容可以是数组，也可以是 {"cases": [...]}。
func LoadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cases: %w", err)
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err == nil {
		return validateCases(cases)
	}
	var envelope struct {
		Cases []Case `json:"cases"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse cases JSON: %w", err)
	}
	return validateCases(envelope.Cases)
}

// LoadRuns 从 JSON 文件读取 Agent Trace。
func LoadRuns(path string) ([]Run, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runs: %w", err)
	}
	var runs []Run
	if err := json.Unmarshal(data, &runs); err == nil {
		return runs, nil
	}
	var envelope struct {
		Runs []Run `json:"runs"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse runs JSON: %w", err)
	}
	return envelope.Runs, nil
}

// LoadPaths 从 JSON 文件读取 Path 定义，供路径设计阶段和数据集校验使用。
func LoadPaths(path string) ([]PathDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read paths: %w", err)
	}
	var paths []PathDefinition
	if err := json.Unmarshal(data, &paths); err == nil {
		return validatePaths(paths)
	}
	var envelope struct {
		Paths []PathDefinition `json:"paths"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parse paths JSON: %w", err)
	}
	return validatePaths(envelope.Paths)
}

func validatePaths(paths []PathDefinition) ([]PathDefinition, error) {
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path.PathID == "" {
			return nil, fmt.Errorf("path_id is required")
		}
		if seen[path.PathID] {
			return nil, fmt.Errorf("duplicate path_id: %s", path.PathID)
		}
		seen[path.PathID] = true
		seenNodes := map[string]bool{}
		for _, node := range path.Nodes {
			if node.ID == "" {
				return nil, fmt.Errorf("node id is required in %s", path.PathID)
			}
			if seenNodes[node.ID] {
				return nil, fmt.Errorf("duplicate node %s in %s", node.ID, path.PathID)
			}
			seenNodes[node.ID] = true
		}
	}
	return paths, nil
}

func validateCases(cases []Case) ([]Case, error) {
	seen := make(map[string]bool, len(cases))
	for _, c := range cases {
		if c.ID == "" {
			return nil, fmt.Errorf("case_id is required")
		}
		if c.Expected.PathID == "" && len(c.Expected.Skills) == 0 && len(c.Expected.ExecutionChain) == 0 {
			return nil, fmt.Errorf("expected.path_id is required for %s", c.ID)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("duplicate case_id: %s", c.ID)
		}
		seen[c.ID] = true
	}
	return cases, nil
}

// Evaluate 比较预期用例和实际 Trace，生成跨 Skill 的诊断报告。
func Evaluate(cases []Case, runs []Run, skillVersion string) Report {
	runByCase := make(map[string]Run, len(runs))
	for _, run := range runs {
		runByCase[run.CaseID] = run
	}

	report := Report{SkillVersion: skillVersion}
	expectedPaths, expectedNodes := map[string]bool{}, map[string]bool{}
	coveredPaths, coveredNodes := map[string]bool{}, map[string]bool{}
	pathMatches, pathChecks := 0, 0
	nodeChecks, nodePasses := 0, 0
	outputChecks, outputPasses := 0, 0
	skillChecks, skillPasses := 0, 0
	chainChecks, chainPasses := 0, 0
	noticeChecks, noticePasses := 0, 0

	for _, c := range cases {
		nodeSpecs := c.Expected.NodeSpecs
		expectedNodeIDs := c.Expected.Nodes
		if len(nodeSpecs) > 0 {
			expectedNodeIDs = make([]string, 0, len(nodeSpecs))
			for _, spec := range nodeSpecs {
				expectedNodeIDs = append(expectedNodeIDs, spec.ID)
			}
		}
		for _, node := range expectedNodeIDs {
			expectedNodes[node] = true
		}
		if c.Expected.PathID != "" {
			expectedPaths[c.Expected.PathID] = true
			pathChecks++
		}

		result := CaseResult{CaseID: c.ID, Name: c.Name, Expected: c.Expected}
		run, ok := runByCase[c.ID]
		if !ok {
			result.Failures = append(result.Failures, Failure{Code: "missing_run", Message: "未找到实际执行 Trace"})
		} else {
			result.Actual = &run
			for _, expectedSkill := range c.Expected.Skills {
				if !expectedSkill.Required {
					continue
				}
				skillChecks++
				if contains(run.SelectedSkills, expectedSkill.Name) {
					skillPasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "skill_not_selected", Message: "未命中预期 Skill", FailedNode: expectedSkill.Name})
				}
			}
			for _, forbiddenSkill := range c.Expected.ForbiddenSkills {
				if contains(run.SelectedSkills, forbiddenSkill) {
					result.Failures = append(result.Failures, Failure{Code: "forbidden_skill", Message: "命中了禁止 Skill", FailedNode: forbiddenSkill})
				}
			}

			actualPath := run.ActualPath
			if len(run.Trace) > 0 {
				actualPath = tracePath(run.Trace)
			}
			if c.Expected.PathID != "" {
				if contains(actualPath, c.Expected.PathID) {
					pathMatches++
					coveredPaths[c.Expected.PathID] = true
				} else {
					result.Failures = append(result.Failures, Failure{Code: "path_mismatch", Message: "实际路径未命中预期 Path", ExpectedPath: c.Expected.PathID, ActualPath: joinPath(actualPath)})
				}
			}

			actualChain := run.ExecutionChain
			if len(actualChain) == 0 && len(run.Trace) > 0 {
				actualChain = traceChain(run.Trace)
			}
			for _, expectedStep := range c.Expected.ExecutionChain {
				if !expectedStep.Required {
					continue
				}
				chainChecks++
				if chainContains(actualChain, expectedStep.ID) {
					chainPasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "chain_step_missing", Message: "完整执行链路缺少预期步骤", FailedNode: expectedStep.ID})
				}
				if expectedStep.Tool != "" && !contains(run.ToolCalls, expectedStep.Tool) {
					result.Failures = append(result.Failures, Failure{Code: "tool_missing", Message: "执行链路要求的工具未被调用", FailedNode: expectedStep.ID})
				}
			}
			for _, notice := range c.Expected.Notices {
				noticeChecks++
				if contains(run.AcknowledgedNotices, notice) {
					noticePasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "notice_missed", Message: "Skill 注意事项未被确认或遵守", FailedNode: notice})
				}
			}

			for index, node := range expectedNodeIDs {
				nodeChecks++
				if contains(actualPath, node) {
					nodePasses++
					coveredNodes[node] = true
				} else {
					result.Failures = append(result.Failures, Failure{Code: "node_missing", Message: "实际路径缺少预期节点", FailedNode: node})
				}
				if index < len(nodeSpecs) && nodeSpecs[index].Tool != "" && !contains(run.ToolCalls, nodeSpecs[index].Tool) {
					result.Failures = append(result.Failures, Failure{Code: "tool_missing", Message: "节点要求的工具未被调用", FailedNode: nodeSpecs[index].ID})
				}
			}
			for _, node := range c.Expected.ForbiddenNodes {
				if contains(actualPath, node) {
					result.Failures = append(result.Failures, Failure{Code: "forbidden_node", Message: "实际路径触发了禁止节点", FailedNode: node})
				}
			}

			outputText := run.OutputText
			if outputText == "" && run.Output != nil {
				if encoded, err := json.Marshal(run.Output); err == nil {
					outputText = string(encoded)
				}
			}
			if c.Expected.OutputType != "" {
				outputChecks++
				if run.OutputType == c.Expected.OutputType {
					outputPasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "output_mismatch", Message: "输出类型与预期不一致"})
				}
			}
			if c.Expected.OutputSpec.Type != "" {
				outputChecks++
				if run.OutputType == c.Expected.OutputSpec.Type {
					outputPasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "output_type_mismatch", Message: "最终输出类型不符合预期"})
				}
			}
			for _, text := range c.Expected.OutputSpec.Contains {
				outputChecks++
				if containsText(outputText, text) {
					outputPasses++
				} else {
					result.Failures = append(result.Failures, Failure{Code: "output_content_missing", Message: "最终输出缺少预期内容", FailedNode: text})
				}
			}
			for _, text := range c.Expected.OutputSpec.NotContain {
				if containsText(outputText, text) {
					result.Failures = append(result.Failures, Failure{Code: "output_content_forbidden", Message: "最终输出包含禁止内容", FailedNode: text})
				}
			}
		}
		result.Passed = len(result.Failures) == 0
		report.Results = append(report.Results, result)
	}

	passed := countPassed(report.Results)
	report.Metrics = Metrics{
		TotalCases: len(cases), PassedCases: passed, FailedCases: len(cases) - passed,
		PathCoverage: ratio(len(coveredPaths), len(expectedPaths)), NodeCoverage: ratio(len(coveredNodes), len(expectedNodes)),
		PathMatchRate: ratio(pathMatches, pathChecks), NodePassRate: ratio(nodePasses, nodeChecks), OutputPassRate: ratio(outputPasses, outputChecks),
		SkillSelectionRate: ratio(skillPasses, skillChecks), ChainPassRate: ratio(chainPasses, chainChecks), NoticePassRate: ratio(noticePasses, noticeChecks),
	}
	return report
}

func tracePath(trace []TraceEvent) []string {
	path := make([]string, 0, len(trace))
	for _, event := range trace {
		if event.NodeID != "" {
			path = append(path, event.NodeID)
		}
	}
	return path
}

func traceChain(trace []TraceEvent) []ChainStep {
	chain := make([]ChainStep, 0, len(trace))
	for _, event := range trace {
		if event.NodeID != "" {
			chain = append(chain, ChainStep{ID: event.NodeID, Kind: event.Kind, Tool: event.Tool, Required: true})
		}
	}
	return chain
}

func chainContains(chain []ChainStep, id string) bool {
	for _, step := range chain {
		if step.ID == id {
			return true
		}
	}
	return false
}

func containsText(text, want string) bool {
	return want != "" && strings.Contains(text, want)
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func joinPath(path []string) string {
	return fmt.Sprintf("%v", path)
}

func ratio(value, total int) float64 {
	if total == 0 {
		return -1
	}
	return float64(value) / float64(total)
}

func countPassed(results []CaseResult) int {
	count := 0
	for _, result := range results {
		if result.Passed {
			count++
		}
	}
	return count
}

// SortResults 保证报告顺序稳定，便于版本比较。
func (r *Report) SortResults() {
	sort.Slice(r.Results, func(i, j int) bool { return r.Results[i].CaseID < r.Results[j].CaseID })
}
