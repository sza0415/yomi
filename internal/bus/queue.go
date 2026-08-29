package bus

import "context"

// MessageBus 是整个系统的"中枢"。
//
// 类比电脑主板上的总线：
//   - 任何 channel（CLI/Telegram/Web/...）把入站消息丢到 inbound；
//   - AgentLoop 从 inbound 消费消息、处理完后把结果丢到 outbound；
//   - 任何 channel 监听 outbound 决定要不要把消息发回自己负责的平台。
//
// 用 Go channel 直接实现就够了：天生支持并发安全 + 背压。
type MessageBus struct {
	inbound  chan InboundMessage
	outbound chan OutboundMessage
}

// New 创建一个带缓冲的消息总线。
// buffer 给小一点（比如 64）就行，目的是避免突发流量时阻塞生产者。
func New(buffer int) *MessageBus {
	if buffer <= 0 {
		buffer = 64
	}
	return &MessageBus{
		inbound:  make(chan InboundMessage, buffer),
		outbound: make(chan OutboundMessage, buffer),
	}
}

// PublishInbound 由 channel 调用，把入站消息推进 bus。
// 使用 ctx 是为了在系统关停时不会被永久阻塞。
func (b *MessageBus) PublishInbound(ctx context.Context, msg InboundMessage) error {
	select {
	case b.inbound <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PublishOutbound 由 AgentLoop 调用，把出站消息推进 bus。
func (b *MessageBus) PublishOutbound(ctx context.Context, msg OutboundMessage) error {
	select {
	case b.outbound <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Inbound 给 AgentLoop 监听用：只读视角。
func (b *MessageBus) Inbound() <-chan InboundMessage { return b.inbound }

// Outbound 给 channel 监听用：只读视角。
func (b *MessageBus) Outbound() <-chan OutboundMessage { return b.outbound }
