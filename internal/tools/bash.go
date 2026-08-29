package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

var bashParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"command": {
			"type": "string",
			"description": "要在沙盒中执行的 bash 脚本。工作目录是挂载进容器的工作区 /work。"
		}
	},
	"required": ["command"],
	"additionalProperties": false
}`)

// BashTool runs a bash script inside a disposable Docker sandbox.
type BashTool struct {
	sandbox *Sandbox
}

// NewBash creates a bash tool backed by sandbox. The sandbox image must contain
// bash (e.g. "bash:5" or a general-purpose distro image).
func NewBash(sandbox *Sandbox) (*BashTool, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("bash: sandbox is nil")
	}
	return &BashTool{sandbox: sandbox}, nil
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Description() string {
	return "在隔离的 Docker 沙盒中执行 bash 命令（如运行测试、查看文件、处理数据）。工作目录挂载为工作区，默认无网络。命令出错会返回退出码而非中断。"
}

func (t *BashTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), bashParameters...)
}

type bashArguments struct {
	Command string `json:"command"`
}

// Execute pipes the script into `bash -s` so no shell escaping of the command
// is required and injection through argv is impossible.
func (t *BashTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if t == nil || t.sandbox == nil {
		return "", fmt.Errorf("bash: tool is not initialized")
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("bash: arguments must be valid JSON")
	}

	var args bashArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("bash: decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("bash: command is required")
	}

	return t.sandbox.Run(ctx, []string{"bash", "-s"}, args.Command)
}
