// Command skill-review 根据测试用例和 Agent Trace 生成 Skill 评审报告。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ziangsun/szabot/internal/skillreview"
)

func main() {
	casesPath := flag.String("cases", "", "测试用例 JSON 文件")
	pathsPath := flag.String("paths", "", "Path 定义 JSON 文件，可选，用于校验 Path 模型")
	runsPath := flag.String("runs", "", "实际执行 Trace JSON 文件")
	runtimeTracePath := flag.String("trace", "", "单个 yomi 运行时 JSONL Trace 文件")
	mappingPath := flag.String("trace-mapping", "", "运行时 Trace 到 Skill Node 的映射 JSON 文件")
	traceCaseID := flag.String("trace-case-id", "runtime-trace", "运行时 Trace 转换后的 Case ID")
	serve := flag.Bool("serve", false, "启动本地评审仪表盘")
	addr := flag.String("addr", ":8090", "仪表盘监听地址")
	workspace := flag.String("workspace", ".", "workspace 根目录，Skill 读写沙盒")
	skillsDir := flag.String("skills", "", "技能目录，默认 <workspace>/skills")
	markdownPath := flag.String("markdown", "", "Markdown 报告输出路径，默认 stdout")
	jsonPath := flag.String("json", "", "JSON 报告输出路径，可选")
	version := flag.String("skill-version", "", "被评审的 Skill 版本")
	flag.Parse()

	if !*serve && *runsPath == "" && *runtimeTracePath == "" {
		fmt.Fprintln(os.Stderr, "用法：skill-review -cases cases.json -runs runs.json [-markdown report.md] [-json report.json]")
		os.Exit(2)
	}
	var cases []skillreview.Case
	var runs []skillreview.Run
	var paths []skillreview.PathDefinition
	var err error
	if *casesPath != "" {
		cases, err = skillreview.LoadCases(*casesPath)
		if err != nil {
			fatal(err)
		}
	}
	if *pathsPath != "" {
		paths, err = skillreview.LoadPaths(*pathsPath)
		if err != nil {
			fatal(err)
		}
	}
	if *runsPath != "" {
		runs, err = skillreview.LoadRuns(*runsPath)
		if err != nil {
			fatal(err)
		}
	}
	if *runtimeTracePath != "" {
		events, loadErr := skillreview.LoadRuntimeTrace(*runtimeTracePath)
		if loadErr != nil {
			fatal(loadErr)
		}
		mapping := skillreview.TraceMapping{}
		if *mappingPath != "" {
			data, readErr := os.ReadFile(*mappingPath)
			if readErr != nil {
				fatal(readErr)
			}
			if readErr = json.Unmarshal(data, &mapping); readErr != nil {
				fatal(readErr)
			}
		}
		runs = append(runs, skillreview.ProjectRuntimeTrace(events, *traceCaseID, mapping))
	}
	report := skillreview.Evaluate(cases, runs, *version)
	report.SortResults()
	if *serve {
		api := newSkillsAPI(*workspace, *skillsDir)
		if err := serveDashboard(*addr, report, paths, api); err != nil {
			fatal(err)
		}
		return
	}

	if *markdownPath == "" {
		if err := skillreview.WriteMarkdown(os.Stdout, report); err != nil {
			fatal(err)
		}
	} else if err := writeMarkdownFile(*markdownPath, report); err != nil {
		fatal(err)
	}
	if *jsonPath != "" {
		file, err := os.Create(*jsonPath)
		if err != nil {
			fatal(err)
		}
		err = skillreview.WriteJSON(file, report)
		closeErr := file.Close()
		if err != nil {
			fatal(err)
		}
		if closeErr != nil {
			fatal(closeErr)
		}
	}
}

func writeMarkdownFile(path string, report skillreview.Report) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return skillreview.WriteMarkdown(file, report)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "skill-review:", err)
	os.Exit(1)
}
