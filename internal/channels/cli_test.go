package channels

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
)

// syncBuffer 是并发安全的 bytes.Buffer，writeLoop 在独立 goroutine 写、
// 测试主 goroutine 读，需要加锁。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor 轮询直到 out 包含 want 或超时。
func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q; got %q", want, out.String())
}

// TestWriteLoopStreamsDeltas 验证：多条 Delta 分片被无换行拼接在同一个
// "yomi> " 前缀之后，Done 到达后补换行与提示符。
func TestWriteLoopStreamsDeltas(t *testing.T) {
	b := bus.New(16)
	out := &syncBuffer{}
	c := &CLIChannel{ID: "cli", Bus: b, In: strings.NewReader(""), Out: out}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.writeLoop(ctx)

	send := func(m bus.OutboundMessage) {
		m.ChannelID = "cli"
		if err := b.PublishOutbound(ctx, m); err != nil {
			t.Fatalf("publish error: %v", err)
		}
	}

	send(bus.OutboundMessage{Text: "你", Delta: true})
	send(bus.OutboundMessage{Text: "好", Delta: true})
	send(bus.OutboundMessage{Text: "世界", Delta: true})
	send(bus.OutboundMessage{Done: true})

	waitFor(t, out, "\nyomi> 你好世界\n> ")

	// 前缀只应出现一次（增量没有各自换行）。
	if got := strings.Count(out.String(), "yomi> "); got != 1 {
		t.Fatalf("prefix count = %d, want 1; got output %q", got, out.String())
	}
}

// TestWriteLoopIgnoresOtherChannels 验证只处理属于自己 ChannelID 的消息。
func TestWriteLoopIgnoresOtherChannels(t *testing.T) {
	b := bus.New(16)
	out := &syncBuffer{}
	c := &CLIChannel{ID: "cli", Bus: b, In: strings.NewReader(""), Out: out}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.writeLoop(ctx)

	_ = b.PublishOutbound(ctx, bus.OutboundMessage{ChannelID: "other", Text: "nope", Delta: true})
	_ = b.PublishOutbound(ctx, bus.OutboundMessage{ChannelID: "cli", Text: "yes", Delta: true})
	_ = b.PublishOutbound(ctx, bus.OutboundMessage{ChannelID: "cli", Done: true})

	waitFor(t, out, "yes")
	if strings.Contains(out.String(), "nope") {
		t.Fatalf("should not print other channel's message; got %q", out.String())
	}
}
