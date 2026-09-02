# yomi Web UI

这是独立于 Go channel 包的 Vue 3 + TypeScript + Vite 前端工程。Go 后端只提供
HTTP API 与 SSE；前端开发、构建和预览全部在本目录完成。`node_modules` 与
`dist` 都只保留在本地。

```bash
npm install
npm run typecheck
npm run build
```

本地开发时先启动 yomi Web 服务，再在此目录运行 `npm run dev`。Vite 默认把
`/api`（包括 SSE 连接）代理到 `http://127.0.0.1:8080`；后端使用其他端口时，
通过 `VITE_YOMI_API_TARGET` 指定完整地址：

```powershell
$env:VITE_YOMI_API_TARGET = "http://127.0.0.1:18080"
npm run dev
```

`npm run build` 输出到本目录的 `dist`，`npm run preview` 同样会把 `/api` 代理到
`VITE_YOMI_API_TARGET`（默认 `http://127.0.0.1:8080`）。

主要目录：

- `src/App.vue`：会话、SSE 流和顶层页面状态
- `src/components/ChatMessage.vue`：消息正文和处理过程组合
- `src/components/AgentActivity.vue`：思考、工具调用及结果聚合面板
- `src/components/MarkdownContent.vue`：Markdown 解析与 HTML 安全过滤
- `src/components/TraceWorkspace.vue`：运行追踪
- `src/components/ConfigWorkspace.vue`：配置管理
