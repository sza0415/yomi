package providers

import "context"

// EchoProvider 是一个"假"的 Provider：你说啥它回啥。
//
// 为什么要它？
//   - 第一阶段我们关心的是"消息能不能在 channel/bus/loop/runner 之间正确流动"，
//     而不是"LLM 回得好不好"；
//   - 用 Echo 可以零成本（不用 API key、不用网络）验证整条链路是通的；
//   - 等链路稳了，再换成真的 OpenAI 兼容 Provider，骨架一行不用改。
type EchoProvider struct{}

// Name 返回 provider 的名字，用于日志。
func (EchoProvider) Name() string { return "echo" }

// Chat 把最后一条 user 消息原样回声回去。
func (EchoProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == RoleUser {
			last = req.Messages[i].Content
			break
		}
	}
	return ChatResponse{Content: "echo: " + last}, nil
}
