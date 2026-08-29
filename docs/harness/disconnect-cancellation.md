# Web 断连与显式任务取消

Web 的 SSE 连接只负责接收 Agent 事件，连接生命周期不等于 Run 生命周期。浏览器刷新、切后台、网络切换、代理回收连接和页面关闭都可能造成 SSE 断连；这些情况不应自动取消仍在执行的模型或工具任务。用户需要停止任务时，由 Web 前端显式调用取消 API。

## 生命周期边界

Web 请求分为三条独立链路：

| 链路 | 接口 | 作用 | 断开后的行为 |
| --- | --- | --- | --- |
| 发送 | `POST /api/send` | 将消息写入 `MessageBus` | 请求结束不影响 Run |
| 接收 | `GET /api/stream?session=...` | 接收 SSE/AG-UI 事件 | 只移除订阅者，EventSource 自动重连 |
| 控制 | `POST /api/cancel` | 显式停止当前 Session 的 Run | 调用 `Loop.CancelSession` |

```mermaid
flowchart LR
    SEND["POST /api/send"] --> BUS[(MessageBus)]
    BUS --> LOOP["AgentLoop"]
    LOOP --> RUN["Run context"]
    SSE["GET /api/stream"] -."只接收事件".-> LOOP
    SSE -."断连 / 重连不取消".-> RUN
    CANCEL["POST /api/cancel"] -->|SessionID| LOOP
    LOOP -->|cancel()| RUN
    RUN --> PROVIDER["Runner / Provider / Tool"]
```

CLI 的 `Ctrl+C` 和服务关闭仍然使用进程级根 `context`。根 context 取消时，所有 Run 的子 context 都会收到取消信号；这是进程生命周期控制，不是 Web 客户端断连控制。

## Run context 与取消注册表

每个 Run 从 Loop 的根 context 派生一个独立的可取消 context，并在开始加载历史或构建上下文前登记取消句柄：

```go
runCtx, cancel := context.WithCancel(ctx)
defer cancel()

handle := &runHandle{run: run, cancel: cancel}
l.registerRun(in.SessionID, handle)
defer l.unregisterRun(in.SessionID, handle)

runCtx = withRoute(runCtx, in.SessionID, in.ChannelID)
result, err := l.Runner.RunCollect(runCtx, messages, sink)
```

提前登记很重要。上下文加载、rolling summary 和模型请求都可能耗时，显式取消必须能覆盖整个 Run，而不是只覆盖进入 `Runner` 之后的阶段。

`Loop.CancelSession(sessionID)` 查找当前 Session 的 Run 句柄并调用 `cancel()`。它是显式用户取消入口，包括 Run 正在等待 `ask_user_question` 回答时；找不到运行中的 Run 时安全返回。取消沿 context 传播到 Runner、Provider 和工具，最终映射为 `RunCancelled`，未完成回合不会写入 Conversation。

## Web API

```http
POST /api/cancel
Content-Type: application/json

{"session":"web:..."}
```

成功响应：

```json
{"session":"web:...","cancelled":true}
```

`cancelled: true` 表示取消请求已被接受；Session 当前没有运行中的 Run 时也返回成功。取消是否真正终止下游工作，以 Run Snapshot 和 Trace 的 `cancelled` 状态为准。

取消是 Go `context` 的协作式信号，不会强制杀死执行 goroutine。Runner/工具返回后，Loop 会把 Run 标记为 `cancelled`，并额外发送一条脱离已取消 Run Context 的 `Done` 收尾事件，确保 Web 的 SSE/AG-UI 状态机完成本轮清理，再处理同一 Session 队列中的下一条消息。

## 前端行为

用户提交消息后，Web 页面显示“取消”按钮。按钮调用 `POST /api/cancel`，成功后清理进行中的流式显示并恢复输入框。SSE 断连时页面继续使用 EventSource 自动重连，不发送取消请求。

## 关停行为

`cmd/szabot` 将同一个进程级 context 传给 Loop 和 WebChannel。收到 `Ctrl+C` 或服务关闭信号后：

1. 根 context 被取消；
2. 所有 Run context 的 `Done()` 关闭；
3. Runner、Provider 和工具按各自的 context 处理中止；
4. Web server 优雅关闭，SSE 连接结束。

## 测试覆盖

- `TestCancelSessionInterruptsRunner`：显式取消能中断下游 Provider，并注销运行句柄；
- `TestCancelSessionCancelsPending`：显式取消能中止等待用户回答的 Run；
- `TestHandleCancelCallsOnCancel`：Web 取消 API 将正确的 SessionID 传给回调；
- `TestDisconnectDoesNotCancel`：SSE 订阅断开不会调用取消回调；
- `TestMultiTabDisconnectOnlyRemovesOne`：多标签页断开一个连接只移除该连接。
