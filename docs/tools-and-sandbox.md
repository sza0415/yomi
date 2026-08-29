# 工具与 Docker 沙盒

本文说明 yomi 当前内置工具的注册方式、工作区边界、联网风险，以及 `bash` / `python` 使用的 Docker 沙盒模型。

在 `SZABOT_PERMISSION_MODE=safe` 下，`bash`、`python`、`web_fetch`、`web_search`
以及写入类工具首次调用会请求审批。可选择 `Allow once`、`Allow always` 或 `Deny`；
`Allow always` 仅在当前 yomi 进程内按工具名记忆，进程重启后不会保留。

## 1. 工具注册

主程序启动时默认注册：

- 文件工具：`read_file`、`write_file`、`edit_file`、`list_dir`、`glob`、`grep`；
- 网页读取：`web_fetch`；
- 状态工具：`todo_write`；
- 用户交互：`ask_user_question`。

条件注册：

- 设置 `TAVILY_API_KEY` 后注册 `web_search`；
- 设置 `SZABOT_SANDBOX` 且 Docker CLI 可用后注册 `bash` 和 `python`。

条件工具初始化失败时，yomi 会打印提示并继续启动，不影响其他工具。

## 2. 工作区

`cmd/szabot` 使用启动时的当前工作目录作为 workspace：

```bash
cd /path/to/project
go run /path/to/yomi/cmd/szabot
```

文件工具与 Docker 沙盒都以这个目录为边界。启动位置错误意味着工具可能得到错误或过大的工作区，因此应先进入目标项目目录。

### 2.1 通用路径检查

文件工具首先解析 workspace 的绝对路径和符号链接，再要求工具参数使用相对路径。`joinInWorkspace` 会拒绝：

- 空路径；
- 绝对路径；
- 清理后通过 `..` 逃出 workspace 的路径。

这是一层词法边界，不代表所有工具都具有相同的符号链接保护。

### 2.2 各文件工具的边界

| 工具 | 主要行为 | 符号链接边界 |
|---|---|---|
| `read_file` | 最多读取 32 KiB UTF-8 文本 | 解析目标符号链接并拒绝逃出 workspace |
| `edit_file` | 对现有文件做精确字符串替换 | 解析目标符号链接并拒绝逃出 workspace |
| `write_file` | 创建父目录并覆盖写 UTF-8 文本 | 当前只做词法路径检查 |
| `list_dir` | 列出目录，可递归 | 路径入口受 workspace 检查；遍历行为仍需谨慎对待符号链接 |
| `glob` | 按文件名模式查找 | 在 workspace 内遍历 |
| `grep` | 按正则搜索文本 | 在 workspace 内遍历 |

`write_file` 当前没有在写入前对目标或已存在父目录执行 `EvalSymlinks`。如果 workspace 内存在指向外部位置的符号链接，例如：

```text
workspace/export -> /sensitive/location
```

那么写入 `export/file.txt` 可能影响 workspace 外部。不要在提供写工具的 workspace 中放置此类链接，尤其不要向不可信用户开放工具调用入口。

## 3. 联网工具

### 3.1 `web_search`

`web_search` 使用 Tavily，需要：

```bash
export TAVILY_API_KEY=tvly-xxxx
```

未设置时不会注册该工具。

### 3.2 `web_fetch`

`web_fetch` 无需 API Key，接受绝对 HTTP/HTTPS URL。当前实现：

- HTTP 超时 20 秒；
- 最多读取响应体 2 MiB；
- 提取标题和可见文本；
- 最多返回 20,000 个字符；
- 跳过 `script`、`style`、`noscript` 和 `head`。

当前只检查 URL scheme 和 host，没有限制：

- `localhost` 和回环地址；
- RFC1918 等私网地址；
- 云平台元数据地址；
- DNS 解析后的内网地址；
- HTTP 重定向后的目标。

因此它存在 SSRF 风险。不要将带有 `web_fetch` 的 Web 服务暴露给不可信用户，也不要把“仅允许 HTTP/HTTPS”误解为“仅能访问公共互联网”。

## 4. 启用 Docker 沙盒

```bash
export SZABOT_SANDBOX=1

# 可选配置
export SZABOT_SANDBOX_NETWORK=1
export SZABOT_PYTHON_IMAGE=python:3.12-slim
export SZABOT_BASH_IMAGE=debian:stable-slim
export SZABOT_SANDBOX_TMP_SIZE=512m

go run ./cmd/szabot
```

默认值：

| 配置 | 默认值 |
|---|---|
| Bash 镜像 | `debian:stable-slim` |
| Python 镜像 | `python:3.12-slim` |
| 网络 | 关闭 |
| 单次执行超时 | 30 秒 |
| 内存 | 512 MB |
| CPU | 1.0 |
| PID 上限 | 256 |
| `/tmp` | 64 MB tmpfs |
| 返回输出 | 最多 64 KiB |

主程序目前只通过环境变量暴露镜像、网络和 `/tmp` 大小；超时、内存、CPU、PID 和输出上限使用代码默认值。

如果 Docker CLI 不在 `PATH` 中，或 CLI 无法连接到正在运行的 Docker daemon，yomi 会在启动时跳过 `bash` 和 `python` 并打印原因。镜像是否存在不在启动时检查，仍会在第一次执行时报告。

## 5. 容器隔离模型

每次调用创建一个新的容器，等价于以下关键参数：

```text
docker run --rm -i
  --memory 512m
  --cpus 1.0
  --pids-limit 256
  -v <host-workspace>:/work
  -w /work
  --read-only
  --tmpfs /tmp:rw,size=64m
  --network none
  <image> ...
```

其中最重要的边界是：

- 容器根文件系统只读；
- `/tmp` 可写但容器退出后消失；
- 默认没有网络；
- **宿主 workspace 以读写方式挂载到 `/work`**。

因此容器命令能够直接修改、创建和删除宿主 workspace 文件。`--read-only` 只限制容器根文件系统，并不会让 bind mount 变成只读。

沙盒能降低命令访问宿主其他路径、滥用网络和无限消耗资源的风险，但不能防止：

- 删除或破坏 workspace 文件；
- 读取 workspace 中的密钥和配置；
- 修改源码、Git 工作树或 sessionlogs；
- 在启用网络后上传 workspace 内容；
- 利用 Docker daemon、镜像或宿主平台自身漏洞。

应配合 Git、备份、最小 workspace、可信镜像和本机访问控制使用。

## 6. 镜像选择

`debian:stable-slim` 只有基础 shell 环境，通常没有 Go、Git、Make 等开发工具。命令能否执行取决于镜像内容。

需要 Go 工具链时可以使用：

```bash
docker pull golang:1.22
export SZABOT_BASH_IMAGE=golang:1.22
```

默认网络关闭，因此不能依赖容器运行时执行 `apt install`、下载 Go 或拉取依赖。应提前准备镜像与依赖缓存，或者在理解风险后设置：

```bash
export SZABOT_SANDBOX_NETWORK=1
```

启用网络后，容器可以访问网络以及 Docker 网络环境所允许的目标，不应视为受信任执行环境。

## 7. macOS 安装 Docker

Apple Silicon 和 Intel Mac 均可通过 Homebrew 安装 Docker Desktop：

```bash
brew install --cask docker
open -a Docker
```

首次启动需要完成 Docker Desktop 初始化。确认客户端和 daemon 都可用：

```bash
docker version
```

可选地提前拉取默认镜像：

```bash
docker pull debian:stable-slim
docker pull python:3.12-slim
```

启用后启动 yomi：

```bash
export SZABOT_SANDBOX=1
go run ./cmd/szabot
```

正常注册时会输出类似：

```text
sandbox tools enabled: bash(debian:stable-slim) python(python:3.12-slim) network=false
```

这只表示工具已注册，不表示镜像包含 `go`、`git`、`make` 等命令，也不保证 Docker daemon 或镜像在首次执行时一定可用。

## 8. 使用建议

- 从最小必要目录启动 yomi，不要把用户主目录作为 workspace；
- 不要在 workspace 放置指向敏感目录的可写符号链接；
- 不可信环境不要启用 Web、`web_fetch` 或 Docker 执行工具；
- Docker 镜像使用固定、可信版本，不要随意运行未知镜像；
- 默认保持 `SZABOT_SANDBOX_NETWORK` 关闭；
- 执行前保存或提交重要变更，便于恢复 workspace；
- 不要在 workspace 中保存长期有效的生产密钥。

## 9. 相关实现

- `cmd/szabot/main.go`：工具注册和环境变量配置；
- `internal/tools/workspace.go`：workspace 解析和词法路径边界；
- `internal/tools/read_file.go`：读取与符号链接检查；
- `internal/tools/write_file.go`：覆盖写入；
- `internal/tools/edit_file.go`：精确替换与符号链接检查；
- `internal/tools/web_fetch.go`：网页抓取；
- `internal/tools/sandbox.go`：Docker 参数和资源限制；
- `internal/tools/bash.go`、`internal/tools/python.go`：执行工具封装。
