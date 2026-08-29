package skillreview

import (
	"encoding/json"
	"fmt"
	"io"
)

// WriteJSON 将报告写为格式化 JSON。
func WriteJSON(w io.Writer, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// WriteMarkdown 将报告写为开发者可读的诊断报告。
func WriteMarkdown(w io.Writer, report Report) error {
	m := report.Metrics
	f := func(value float64) string {
		if value < 0 {
			return "不适用"
		}
		return fmt.Sprintf("%.1f%%", value*100)
	}
	if _, err := fmt.Fprintf(w, "# Skill 评审报告\n\n"); err != nil {
		return err
	}
	if report.SkillVersion != "" {
		if _, err := fmt.Fprintf(w, "- Skill 版本：`%s`\n", report.SkillVersion); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "- 用例：%d，总体通过：%d，失败：%d\n\n", m.TotalCases, m.PassedCases, m.FailedCases); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "## 指标\n\n| 指标 | 结果 |\n|---|---:|\n| Skill 命中率 | %s |\n| Path 覆盖率 | %s |\n| Node 覆盖率 | %s |\n| 路径匹配率 | %s |\n| 链路通过率 | %s |\n| 节点通过率 | %s |\n| 注意事项通过率 | %s |\n| 输出通过率 | %s |\n\n", f(m.SkillSelectionRate), f(m.PathCoverage), f(m.NodeCoverage), f(m.PathMatchRate), f(m.ChainPassRate), f(m.NodePassRate), f(m.NoticePassRate), f(m.OutputPassRate)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "## 用例结果\n\n| Case | 状态 | 失败节点 | 问题 |\n|---|---|---|---|"); err != nil {
		return err
	}
	for _, result := range report.Results {
		status := "通过"
		if !result.Passed {
			status = "失败"
		}
		node, message := "-", "-"
		if len(result.Failures) > 0 {
			node, message = result.Failures[0].FailedNode, result.Failures[0].Message
			if node == "" {
				node = "-"
			}
		}
		if _, err := fmt.Fprintf(w, "| `%s` | %s | `%s` | %s |\n", result.CaseID, status, node, message); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, "\n## 失败详情"); err != nil {
		return err
	}
	for _, result := range report.Results {
		if result.Passed {
			continue
		}
		if _, err := fmt.Fprintf(w, "### `%s` %s\n\n", result.CaseID, result.Name); err != nil {
			return err
		}
		for _, failure := range result.Failures {
			if _, err := fmt.Fprintf(w, "- `%s`：%s", failure.Code, failure.Message); err != nil {
				return err
			}
			if failure.FailedNode != "" {
				if _, err := fmt.Fprintf(w, "（节点：`%s`）", failure.FailedNode); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
	}
	return nil
}
