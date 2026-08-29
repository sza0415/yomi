package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Asker 是"向用户提问并等待回答"的能力抽象。
//
// 为什么放在 tools 包：ask_user_question 工具依赖它，而具体实现（走 bus
// 双向通道）在 agent.Loop 里。把接口定义在这里，agent 包实现它、Runner 注入它，
// 就避免了 tools → agent 的 import 循环。
type Asker interface {
	// Ask 把问题发给用户，阻塞直到用户回答或 ctx 取消。
	// options 是可选的候选项列表（供 channel 渲染成可点击的选项）；为空表示开放式回答。
	Ask(ctx context.Context, question string, options []string) (string, error)
}

// askerKey 是 context 里存放 Asker 的私有 key。
type askerKey struct{}

// WithAsker 把 Asker 放进 ctx。由 Runner 在执行工具前调用，
// 这样工具能取到它而无需改变 Tool.Execute 的签名。
func WithAsker(ctx context.Context, asker Asker) context.Context {
	return context.WithValue(ctx, askerKey{}, asker)
}

// askerFrom 从 ctx 取 Asker。ok 为 false 表示当前没有可用的交互通道。
func askerFrom(ctx context.Context) (Asker, bool) {
	asker, ok := ctx.Value(askerKey{}).(Asker)
	return asker, ok && asker != nil
}

// AskerFromContext exposes the host-controlled interaction channel to policy
// gates. Tool implementations should normally use the private helper above.
func AskerFromContext(ctx context.Context) (Asker, bool) { return askerFrom(ctx) }

var askUserQuestionParameters = json.RawMessage(`{
	"type": "object",
	"properties": {
		"question": {
			"type": "string",
			"description": "要问用户的问题，应清晰、具体。"
		},
		"options": {
			"type": "array",
			"items": {"type": "string"},
			"description": "可选。让用户在其中选择的候选项列表；省略则为开放式回答。"
		}
	},
	"required": ["question"],
	"additionalProperties": false
}`)

// AskUserQuestionTool 向用户提问，用于补充信息、确认需求或让用户在多个选项中选择。
//
// 它本身不碰 stdin/平台细节：只调用 ctx 里注入的 Asker，由 Loop 经 bus 把问题
// 发给对应 channel、再把用户回答喂回来。因此在 CLI/飞书/Web 等任意 channel 通用。
type AskUserQuestionTool struct{}

// NewAskUserQuestion 创建提问工具。它没有内部状态，所有交互都经 ctx 里的 Asker。
func NewAskUserQuestion() (*AskUserQuestionTool, error) {
	return &AskUserQuestionTool{}, nil
}

func (t *AskUserQuestionTool) Name() string { return "ask_user_question" }

func (t *AskUserQuestionTool) Description() string {
	return "向用户提问，用于补充信息、确认需求或让用户在多个选项中选择。会暂停当前任务，把问题发给用户，等用户回复后再继续。"
}

func (t *AskUserQuestionTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), askUserQuestionParameters...)
}

type askUserQuestionArguments struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

// Execute 组装问题文本，通过 Asker 发给用户并阻塞等待回答，把回答返回给模型。
func (t *AskUserQuestionTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("ask_user_question: arguments must be valid JSON")
	}

	var args askUserQuestionArguments
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("ask_user_question: decode arguments: %w", err)
	}
	if strings.TrimSpace(args.Question) == "" {
		return "", fmt.Errorf("ask_user_question: question is required")
	}

	asker, ok := askerFrom(ctx)
	if !ok {
		return "", fmt.Errorf("ask_user_question: no interactive channel available")
	}

	question := strings.TrimSpace(args.Question)
	options := cleanOptions(args.Options)
	answer, err := asker.Ask(ctx, question, options)
	if err != nil {
		return "", fmt.Errorf("ask_user_question: %w", err)
	}

	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "（用户未提供回答）", nil
	}
	return answer, nil
}

// cleanOptions 去掉空白项，返回规整后的候选项列表。
func cleanOptions(options []string) []string {
	clean := make([]string, 0, len(options))
	for _, opt := range options {
		if trimmed := strings.TrimSpace(opt); trimmed != "" {
			clean = append(clean, trimmed)
		}
	}
	return clean
}
