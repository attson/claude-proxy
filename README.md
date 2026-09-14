# claude-proxy

本机 SSE 反向代理，修复 Claude Code CLI 偶发的**工具调用退化 bug**：模型/网关在流式响应里把结构化工具调用采样坏了，导致工具不执行、会话卡住。代理插在 Claude Code 与上游之间，拦截流式 SSE，把退化的调用**重组回合法结构**，使工具照常执行。

**核心原则**：任何自身异常一律 **fail-open**——原样透传，绝不破坏正常请求。

## 修复的两类退化

**第一类：工具调用掉进文本（tool_use → text）**
本应是结构化 `tool_use` 的调用，被序列化成普通文本：

```
count
<invoke name="Bash">
<parameter name="command">...</parameter>
</invoke>
```

（前缀在 `count`/`court` 间摇摆、独立成行）。代理把这段伪 XML 解析出来，重组成真正的 `tool_use` block（前面的正常叙述保留为 text，尾部伪调用转 tool_use）。

**第二类：工具参数被编码坏（input 双重编码 / 坏转义）**
`tool_use` 结构完整，但 `input` 里某个本该是数组/对象的字段被双重编码成 JSON 字符串（如 `AskUserQuestion.questions`），有时内层还有被污染的 `\u` 转义（如 `\u7 ee7`）。代理对已知字段解一层、清洗坏转义还原成合法 JSON。仅限已知坏字段，不误伤本就是 JSON 字符串的字段。

## 用法

### 推荐：`run` 一条命令（不改 settings.json）

```bash
claude-proxy run                    # 等价于 claude,但走代理
claude-proxy run -- --model opus    # -- 之后的参数原样透传给 claude
```

`run` 会自动：
1. 探测代理端口，没跑就后台拉起一个（带救援）。
2. 从 `~/.claude/settings.json` 的 `env.ANTHROPIC_BASE_URL` 读出**真实上游**，让代理转发过去（代理不硬编码任何上游地址）。
3. 给 claude 设 `ANTHROPIC_BASE_URL=http://127.0.0.1:36240`，并用 `--settings` 内联 JSON 覆盖（优先级高于 settings.json）——**完全不改你的 settings 文件**。
4. 前台启动 claude，转发信号，透传退出码。

想直连？正常 `claude` 即可（不走本工具），两者并存。

### 透明替换 claude（alias）

把 `run` 做成 alias，让 `claude` 默认走代理：

```bash
# 1. 软链到 PATH(以后 go build 重建自动生效)
ln -sf "$PWD/claude-proxy" ~/.local/bin/claude-proxy

# 2. ~/.zshrc 或 ~/.bashrc
alias claude='claude-proxy run --'
```

之后 `claude` 自动走代理。想临时用原生 claude：`command claude` 或 `\claude`。回退：注释掉 alias，重开终端。

> alias 覆盖 `claude` 是安全的——alias 只在交互 shell 命令行首解析，代理内部调的仍是 PATH 里的真实 claude 二进制，不会递归。

### 升级

```bash
claude-proxy update          # 检查并升级到最新 Release
claude-proxy update --check  # 只检查有无新版,不下载
claude-proxy version
```

从 GitHub Releases 拉对应平台产物，校验 sha256 后原子替换自身。网络受限时设镜像前缀：`CLAUDE_PROXY_DOWNLOAD_MIRROR=<前缀> claude-proxy update`。

> 若用源码构建（软链本地二进制），升级走 `git pull` + 重新 `go build`，而非 `update`。

### 手动启停 / 分析样本

```bash
./start.sh     # nohup 常驻(仅代理,不启 claude)
./stop.sh

# 分析最新落盘样本(排查退化用)
ls -t ~/.claude-proxy/samples/*.sse | head -1 | xargs claude-proxy analyze
```

## 构建

```bash
go build -o claude-proxy .          # 需 Go 1.23+;零第三方依赖
# 带版本号:
go build -ldflags "-s -w -X main.version=$(git describe --tags)" -o claude-proxy .
```

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `CLAUDE_PROXY_PORT` | 36240 | 监听端口 |
| `CLAUDE_PROXY_UPSTREAM` | `https://api.anthropic.com` | 真实上游 base URL。`run` 会用 settings.json 的 `ANTHROPIC_BASE_URL` 覆盖它;显式设此变量则优先 |
| `CLAUDE_PROXY_PASSTHROUGH` | 0 | =1 纯转发,不进 SSE 拦截 |
| `CLAUDE_PROXY_SAMPLE` | 1 | =1 脱敏落盘样本 |
| `CLAUDE_PROXY_RESCUE` | 0 | =1 开启救援(`run` 拉起时默认开) |
| `CLAUDE_PROXY_REDACT` | 1 | =1 落盘前脱敏 token/凭证;=0 原文落盘(仅磁盘安全时) |
| `CLAUDE_PROXY_SAMPLE_KEEP` | 200 | 样本文件保留数(轮转) |
| `CLAUDE_PROXY_DOWNLOAD_MIRROR` | (空) | update 下载镜像前缀,适配受限网络 |

上游来源优先级：`CLAUDE_PROXY_UPSTREAM` > settings.json 的 `ANTHROPIC_BASE_URL` > 默认官方端。代理有**自指检测**：上游若指向代理自己（回环+同端口）会拒绝启动，防死循环。

## 日志文件

| 文件 | 内容 | 是否含对话内容 |
|---|---|---|
| `~/.claude-proxy/proxy.log` | 启动信息、上游错误、fail-open 提示 | 否(只有 URL) |
| `~/.claude-proxy/audit.log` | 每次救援/检测一行 JSON:结果、工具 name、参数**个数**、block index、样本名 | 否(**不含参数值**) |
| `~/.claude-proxy/samples/*.sse` | 一次流式响应的完整原始 SSE(默认脱敏) | **是**(含模型输出、工具参数;凭证已脱敏) |

## 安全

- 只监听 `127.0.0.1`，不暴露网络。
- 落盘前脱敏 token、`Authorization`/`x-api-key`/`Bearer` 头值、body 里的凭证字段。**代理内存持有真 token 仅用于转发，绝不写盘。**
- 样本目录权限 700，自动轮转。

## 文件

| 文件 | 职责 |
|---|---|
| `config.go` | 配置、上游解析、自指检测 |
| `proxy.go` | ReverseProxy + ModifyResponse,SSE 分流,透传/救援入口 |
| `sse.go` | SSE 事件解析/序列化 |
| `rescue.go` | 退化检测与 tool_use 重组、input 修复接入 |
| `inputfix.go` | input 双重编码 / 坏 `\u` 转义修复 |
| `sample.go` / `redact.go` | 脱敏落盘 + 轮转 |
| `audit.go` | 救援审计日志 |
| `analyze.go` | 离线样本分析(`claude-proxy analyze <file>`) |
| `run.go` | `run` 子命令:读上游、拉起代理、启动 claude |
| `update.go` | 自更新 |
| `main.go` | 子命令分发、serve、健康检查 |
| `spawn_unix.go` / `spawn_windows.go` | 平台相关(后台常驻、信号、进程探测) |
