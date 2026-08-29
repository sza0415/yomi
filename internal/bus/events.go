// Package bus 定义系统中流转的"统一消息"格式。
//
// 设计要点：
//   - 不管消息从 CLI、Telegram 还是 Web 来，进入 bus 之前都被翻译成 InboundMessage；
//   - 不管要发回哪个平台，发出去之前都先变成 OutboundMessage；
//   - 这样 AgentLoop 只需要面对这两种统一类型，不用关心来源/去向。
package bus

import "time"

// InboundMessage 表示从某个 channel 进入系统的一条消息。
type InboundMessage struct {
	// SessionID 用于标识"这是哪一轮对话"，
	// 通常由 channel 决定（CLI 固定一个；Telegram 用 chat_id 等）。
	SessionID string

	// ChannelID 标识消息来自哪个 channel 实例，
	// AgentLoop 处理完后要按这个 ID 把回复送回去。
	ChannelID string

	// UserID 是平台内的用户标识（CLI 可以固定 "local"）。
	UserID string

	// Text 是用户的文本输入。后续要支持图片/文件再扩展即可。
	Text string

	// Time 是消息进入 bus 的时间。
	Time time.Time

	// Meta 用于放各 channel 的原生附加信息，AgentLoop 一般不读。
	Meta map[string]any
}

// OutboundKind 标识一条出站消息承载的是哪一类内容。
//
// 引入它之前，出站流只能表达"正文增量/结束"两种状态，推理过程与工具调用
// 无从区分。channel 侧（CLI/Web）可据此把思考、工具调用、正文分区渲染，
// 而无需猜测 Text 的语义。
//
// 兼容性：Kind 为空（零值 KindAnswer）时等价于旧的"正文"语义，
// 旧 channel 无需改动即可继续把 Text 当正文处理。
type OutboundKind string

const (
	// KindAnswer 是面向用户的正文（默认值，兼容旧行为）。
	KindAnswer OutboundKind = ""
	// KindReasoning 是推理型模型的思考过程增量。
	KindReasoning OutboundKind = "reasoning"
	// KindToolCall 表示模型请求调用某个工具（Text 为可读描述）。
	KindToolCall OutboundKind = "tool_call"
	// KindToolResult 表示某次工具调用的执行结果（Text 为结果摘要）。
	KindToolResult OutboundKind = "tool_result"
	// KindQuestion 表示 agent 正在向用户提问并等待回答（ask_user_question）。
	// channel 收到它应提示用户输入回答，而不能把本轮当作已结束。
	KindQuestion OutboundKind = "question"
)

// OutboundMessage 表示由 agent 产生、要发回某个 channel 的消息。
//
// 流式支持：一段完整回复会被拆成"多条分片 + 一条结束标记"流过 bus：
//   - 分片消息：Delta=true，Text 是本次新增的一小段正文；
//   - 结束消息：Done=true，Text 通常为空（正文已由前面的分片给完）；
//   - 非流式/回退：也可以只发一条 Delta=false、Done=false 的完整消息，
//     channel 直接把它当作一整段回复处理即可（向后兼容）。
//
// 内容类型：Kind 区分正文 / 推理过程 / 工具调用 / 工具结果。默认零值
// (KindAnswer) 表示正文，从而旧 channel 无需改动即可继续工作。
type OutboundMessage struct {
	SessionID string
	ChannelID string
	RunID     string
	AgentID   string
	Sequence  uint64
	Text      string
	// Kind 标识本条消息的内容类型，默认 KindAnswer（正文）。
	Kind OutboundKind
	// Delta 为 true 表示这是一段增量（流式输出中的一小块）。
	Delta bool
	// Done 为 true 表示本轮回复到此结束（流式收尾标记）。
	Done bool
	Time time.Time
	Meta map[string]any

	// 以下字段仅在 Kind 为工具相关时填充，用于让出站侧（如 Web 的 AG-UI
	// 事件翻译）能发出严格合规的 TOOL_CALL_* 事件——这些事件都以 toolCallId
	// 为核心。此前工具调用被格式化成人类可读字符串塞进 Text，call.ID 在那一步
	// 就丢了，出站侧无从配对调用与结果。为不改动核心链路语义，这里以可选字段
	// 的形式把结构化信息透传到出站侧；不关心它们的 channel（如 CLI）可忽略。
	//
	//   - ToolCallID：KindToolCall / KindToolResult 时填，调用与结果据此配对；
	//   - ToolName：  KindToolCall 时填，工具名；
	//   - Arguments： KindToolCall 时填，原始参数 JSON（未经拼串加工）。
	ToolCallID string
	ToolName   string
	Arguments  string
}
