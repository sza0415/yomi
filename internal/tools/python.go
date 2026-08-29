package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

var pythonParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"code": {
			"type": "string",
			"description": "要执行的 Python 代码。在隔离沙盒中运行，工作目录为工作区 /work，标准输出即返回结果。"
		}
	},
	"required": ["code"],
	"additionalProperties": false
}`)

// PythonTool executes Python code inside a disposable Docker sandbox.
// It is the "code interpreter": a place to safely run Python where a crash or
// destructive call cannot affect the host.
type PythonTool struct {
	sandbox *Sandbox
}

// NewPython creates a python tool backed by sandbox. The sandbox image must
// contain a python3 interpreter (e.g. "python:3.12-slim").
func NewPython(sandbox *Sandbox) (*PythonTool, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("python: sandbox is nil")
	}
	return &PythonTool{sandbox: sandbox}, nil
}

func (t *PythonTool) Name() string { return "python" }

func (t *PythonTool) Description() string {
	return "在隔离的 Docker 沙盒中执行 Python 代码（代码解释器）。适合计算、数据处理、快速验证逻辑。工作目录挂载为工作区，默认无网络，出错也不影响宿主机。stdout 即结果。"
}

func (t *PythonTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), pythonParameters...)
}

type pythonArguments struct {
	Code string `json:"code"`
}

// Execute pipes the code into `python3 -` so no source escaping is needed.
func (t *PythonTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.sandbox == nil {
		return "", fmt.Errorf("python: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("python: arguments must be valid JSON")
	}

	var args pythonArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("python: decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Code) == "" {
		return "", fmt.Errorf("python: code is required")
	}

	return t.sandbox.Run(ctx, []string{"python3", "-"}, args.Code)
}
