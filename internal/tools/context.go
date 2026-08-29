package tools

import "context"

// 这里集中放"由 agent 层通过 ctx 注入、供工具读取"的会话上下文。
// 目的：让工具（如 todo_write）能区分不同会话，而不必改变 Tool.Execute 签名，
// 也不必让 tools 包 import agent 包（避免 import 循环）。

// sessionKey 是 ctx 里存放 SessionID 的私有 key。
type sessionKey struct{}

// WithSession 把 SessionID 放进 ctx。由 Runner 在执行工具前调用。
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey{}, sessionID)
}

// sessionFrom 从 ctx 取 SessionID。取不到时返回空串——工具应把它当作默认会话。
func sessionFrom(ctx context.Context) string {
	if s, ok := ctx.Value(sessionKey{}).(string); ok {
		return s
	}
	return ""
}
