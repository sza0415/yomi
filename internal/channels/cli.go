// Package channels 包含各种 channel 实现。
//
// 一个 channel = 一个具体平台的"翻译官"：
//   - 入站：把平台原生消息翻译成 bus.InboundMessage 丢进 bus；
//   - 出站：从 bus 监听 OutboundMessage，挑出 ChannelID == 自己 ID 的，
//     翻译回平台格式发出去。
package channels

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
)

// CLIChannel 是最简单的 channel：从 stdin 读一行 → 入站；从 outbound 读一条 → 打印到 stdout。
//
// 为什么第一个写 CLI？
//   - 没有外部依赖（不需要 API key、不需要 webhook、不需要网络）；
//   - 能 100% 验证整条 channel ⇄ bus ⇄ loop ⇄ runner ⇄ provider 链路；
//   - 等链路稳了，再写 Telegram/飞书/Web，照着这个套路改翻译层即可。
type CLIChannel struct {
	// ID 就是 ChannelID。CLI 场景一般固定一个就行。
	ID string

	// Bus 是消息总线引用。
	Bus *bus.MessageBus

	// In/Out 默认是 stdin/stdout，留出来主要是为了方便写测试。
	In  io.Reader
	Out io.Writer
}

// SessionID 由 channel 决定。CLI 是单人本地终端，固定一个 session 就够。
const cliSessionID = "cli:local"

// Start 起两个 goroutine：一个读入站、一个写出站。
func (c *CLIChannel) Start(ctx context.Context) {
	if c.ID == "" {
		c.ID = "cli"
	}
	if c.In == nil {
		c.In = os.Stdin
	}
	if c.Out == nil {
		c.Out = os.Stdout
	}

	go c.readLoop(ctx)
	go c.writeLoop(ctx)
}

// readLoop：从 stdin 一行行读，翻译成 InboundMessage 推进 bus。
func (c *CLIChannel) readLoop(ctx context.Context) {
	scanner := bufio.NewScanner(c.In)
	// 提前打一个提示符，避免用户看不到输入位置。
	fmt.Fprint(c.Out, "> ")

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			fmt.Fprint(c.Out, "> ")
			continue
		}

		in := bus.InboundMessage{
			ChannelID: c.ID,
			SessionID: cliSessionID,
			UserID:    "local",
			Text:      line,
			Time:      time.Now(),
		}
		if err := c.Bus.PublishInbound(ctx, in); err != nil {
			// ctx 被取消时正常退出，不报噪音。
			return
		}
	}
}

// writeLoop：从 outbound 接消息，挑出属于自己的，按流式标记打印。
//
// 一段回复的到达顺序通常是：可能若干条推理/工具事件 → 若干条正文 Delta →
// 一条 Done（收尾）。
//   - 推理（KindReasoning）：以暗淡的 "· " 前缀成段显示，与正文区分；
//   - 工具调用/结果（KindToolCall/KindToolResult）：各占一行，带 "⚙" 标记；
//   - 正文（KindAnswer）：第一条前打印 "\nyomi> " 前缀，之后直接追加、不换行；
//   - Done 到达时补一个换行并重打输入提示符 "> "；
//   - 兼容非流式：既非 Delta 也非 Done 的整段消息，按老行为一次性打印。
func (c *CLIChannel) writeLoop(ctx context.Context) {
	streaming := false     // 本轮是否已打印过 "yomi> " 前缀（正文）
	reasoningOpen := false // 本轮是否正处于推理输出段
	for {
		select {
		case <-ctx.Done():
			return
		case out, ok := <-c.Bus.Outbound():
			if !ok {
				return
			}
			// 只处理 ChannelID 是自己的消息，其他 channel 的不归我管。
			if out.ChannelID != c.ID {
				continue
			}

			switch {
			case out.Delta && out.Kind == bus.KindReasoning:
				// 推理过程：与正文分区，用暗色前缀提示这是"思考"。
				if !reasoningOpen {
					fmt.Fprint(c.Out, "\n\x1b[2m[思考] ")
					reasoningOpen = true
				}
				fmt.Fprint(c.Out, out.Text)

			case out.Delta && out.Kind == bus.KindToolCall:
				if reasoningOpen {
					fmt.Fprint(c.Out, "\x1b[0m")
					reasoningOpen = false
				}
				fmt.Fprintf(c.Out, "\n\x1b[36m⚙ 调用 %s\x1b[0m", out.Text)

			case out.Delta && out.Kind == bus.KindToolResult:
				fmt.Fprintf(c.Out, "\n\x1b[36m⚙ 结果 %s\x1b[0m", out.Text)

			case out.Delta:
				// 正文增量。切出推理段后再进入正文。
				if reasoningOpen {
					fmt.Fprint(c.Out, "\x1b[0m")
					reasoningOpen = false
				}
				if !streaming {
					fmt.Fprint(c.Out, "\nyomi> ")
					streaming = true
				}
				fmt.Fprint(c.Out, out.Text)

			case out.Done:
				// 收尾：无论中途是否有过增量，都换行并重打提示符。
				if reasoningOpen {
					fmt.Fprint(c.Out, "\x1b[0m")
					reasoningOpen = false
				}
				if !streaming {
					// 没有任何正文的空回复，也给个前缀避免光标错乱。
					fmt.Fprint(c.Out, "\nyomi> ")
				}
				fmt.Fprint(c.Out, "\n> ")
				streaming = false

			default:
				// 非流式的整段消息：保持老行为。
				fmt.Fprintf(c.Out, "\nyomi> %s\n> ", out.Text)
			}
		}
	}
}
