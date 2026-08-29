package agent

import "context"

// routeKey 是 context 里存放"回信地址"（SessionID/ChannelID）的私有 key 类型。
// 用私有类型避免与其他包的 context key 冲突。
type routeKey struct{}

// route 是一次对话的回信地址，随 ctx 从 Loop 传到 Runner 内部的工具。
type route struct {
	sessionID string
	channelID string
}

// withRoute 把回信地址塞进 ctx。由 Loop 在进入 Runner 前调用。
func withRoute(ctx context.Context, sessionID, channelID string) context.Context {
	return context.WithValue(ctx, routeKey{}, route{sessionID: sessionID, channelID: channelID})
}

// routeFrom 从 ctx 取回信地址。ok 为 false 表示当前不在一次对话处理中。
func routeFrom(ctx context.Context) (sessionID, channelID string, ok bool) {
	r, ok := ctx.Value(routeKey{}).(route)
	if !ok {
		return "", "", false
	}
	return r.sessionID, r.channelID, true
}
