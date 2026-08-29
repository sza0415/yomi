# kbcli mock —— 直接用 golang:1.22 跑通影库 CLI 链路

`./kbcli` 是一个**假的影库 CLI**（bash 脚本），用于在有 Docker 的机器上真实闭环
szabot 的 agent loop：`读 skill → 意图识别 → 沙盒 bash 执行 kbcli → 拿到 <text> → 总结`。

它**不连接任何真实后端**，只按 `skills/kbcli` 文档约定的格式伪造 SSE 事件流和最终结果。

## 为什么不需要自定义镜像

szabot 沙盒（见 `internal/tools/sandbox.go`）运行时：

- `-v <工作区>:/work -w /work`：把**工作区挂载到 `/work` 且可读写、可执行**；
- `--read-only`：根文件系统只读（所以不能往 `/usr/local/bin` 写 kbcli）。

既然 `/work` 可执行，**把 `kbcli` 放在工作区根目录即可**，直接用现成的 `golang:1.22`
（Debian 系，自带 bash / coreutils），无需 build 任何镜像。

## 启动 szabot

```bash
export SZABOT_SANDBOX=1
export SZABOT_BASH_IMAGE=golang:1.22
# mock 不需要联网；接真实后端时再打开：
# export SZABOT_SANDBOX_NETWORK=1

# 正常启动 szabot（工作区即本目录，kbcli 会被挂进容器的 /work/kbcli）
```

看到启动日志出现下面这行即代表 bash 工具已挂上：
```
sandbox tools enabled: bash(golang:1.22) python(...) tmp=64m network=false
```

## 模型侧如何调用

容器工作目录是 `/work`，`kbcli` 不在 PATH，用以下任一方式调用：

```bash
# 方式一：绝对路径
/work/kbcli sage ask --agent-id=novel_agent --text "@script-id:SCR_20260817_001，分析改编潜力"

# 方式二：相对路径（-w /work，等价）
./kbcli sage ask --agent-id=novel_agent --text "..."

# 方式三：临时加进 PATH，之后可写裸 kbcli
export PATH=/work:$PATH
kbcli kb-recall --text "庆余年 播放" --scope "/影库知识库/电视剧/项目信息/项目基础信息"
```

## 手动冒烟测试（模拟沙盒的 `bash -s` 灌入方式）

沙盒真实执行方式是 `docker run --rm -i -v <ws>:/work -w /work <image> bash -s`，命令走 stdin：

```bash
WS=/Users/ziangsun/Documents/szabot

echo '/work/kbcli sage ask --agent-id=novel_agent --text "@script-id:SCR_20260817_001，分析改编潜力"' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s

echo '/work/kbcli kb-recall --text "庆余年 播放"' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s

echo '/work/kbcli kb-recall --text "庆余年 播放" --scope "/影库知识库/电视剧/项目信息/项目基础信息"' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s

echo '/work/kbcli kb-search --query "SELECT 项目ID,项目名称 WHERE 项目名称 = \"庆余年\""' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s

echo '/work/kbcli kb-search --database dsj --sql "SELECT dt,vv FROM t"' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s

echo '/work/kbcli kb-search' \
  | docker run --rm -i -v "$WS:/work" -w /work golang:1.22 bash -s   # 期望 InvalidQuery
```

`sage ask` 的 stdout 预期：
```
<thread_id>th_xxxxxxxxxxxx</thread_id><text>该作品为都市悬疑……评级：B+。</text>
```

## 输出契约（与 skill 文档一致）

- `sage ask` → SSE 事件流打到 **stderr**，最终 `<thread_id>...</thread_id><text>...</text>` 打到 **stdout**；按 `agent-id`（novel/market/variety/script）返回不同风格正文。
- `kb-recall` → 无 `--scope` 返回 `<catalog>`（中间态），带 `--scope` 返回 `<text>` 字段段。
- `kb-search` → DSL（`--query`）与 SQL（`--database+--sql`）**互斥**，同传/都不传报 `<error>InvalidQuery</error>`。

## 换成真实 kbcli

保持三个子命令的命令行接口与输出格式（`<thread_id>` / `<text>` / `<catalog>` / `<error>`）不变，
把 `./kbcli` 内部逻辑换成真实 HTTP/SSE 请求即可，skill 文档与 szabot 侧都无需改动。

> 注意：真实后端若单次 >30s，需调大 `internal/tools/sandbox.go` 的 `defaultSandboxTimeout`，
> 或改为轮询式短请求。
