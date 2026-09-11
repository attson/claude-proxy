# claude-proxy

本机 SSE 反向代理,修复 Claude Code CLI 偶发的 **tool_use 退化 bug**:本应结构化的工具调用被序列化成普通文本,形如

```
count
<invoke name="Bash">
<parameter name="command">...</parameter>
</invoke>
```

(前缀在 `count`/`court` 间摇摆、独立成行、无 `<function_calls>` 包裹),导致工具不执行、会话卡住。

代理插在 Claude Code 与上游网关之间,拦截流式 SSE,把退化的 `<invoke>` 文本重组回真正的 `tool_use` block,使工具照常执行。**任何自身异常一律 fail-open**——原样透传,绝不破坏正常请求。

## 已知退化形态(来自真实观测)

- 前缀 `count` / `court`,独立成行,后跟 `<invoke name="...">`。
- 多参数,参数值可能极复杂(含 `&&`、`$VAR`、`"`、`/`、换行、带空格路径)。
- 退化几乎必现于"同一回合里先有解释性 text、再跟工具调用"的场景;纯工具调用回合不退化。

## 两阶段

- **阶段 0/1(当前)**:纯透传 + 脱敏落盘原始 SSE 到 `~/.claude-proxy/samples/`。复现退化后用 `analyze` 分析样本,确认退化的真实 wire 形态(text block 累积出 `<invoke>` 文本 = 可救;tool_use block 被破坏 = 另说)。
- **阶段 2**:据样本填 `rescue.go`,`CLAUDE_PROXY_RESCUE=1` 开启救援。

## 用法

### 推荐:`run` 一条命令(不碰 settings.json)

```bash
# 编译
~/sdk/go1.23.12/bin/go build -o claude-proxy .

# 用代理启动 claude(自动拉起代理 + 开启救援 + 注入 base_url)
./claude-proxy run                    # 等价于 claude
./claude-proxy run -- --model opus    # -- 之后的参数原样透传给 claude
```

原理(参考 claude-tap 的 reverse 模式):
- 探测代理端口,没跑就后台拉起一个(带救援)。
- 设 `ANTHROPIC_BASE_URL=http://127.0.0.1:36240`、`NO_PROXY=127.0.0.1`,删 `CLAUDECODE`/`CLAUDE_CODE_SSE_PORT`。
- **关键**:给 claude 加 `--settings '{"env":{"ANTHROPIC_BASE_URL":"..."}}'`,其优先级高于 `~/.claude/settings.json`,所以 base_url 一定生效 —— **完全不改你的 settings 文件**。
- 前台启动 claude,转发信号,透传退出码。
- 想直连?正常 `claude` 即可(不走本工具),两者并存。

可做 alias:`alias cc='~/GolandProjects/claude-proxy/claude-proxy run --'`

### 手动启停(可选)

```bash
./start.sh     # nohup 常驻(仅代理,不启 claude)
./stop.sh

# 分析最新样本
ls -t ~/.claude-proxy/samples/*.sse | head -1 | xargs ./claude-proxy analyze
```

## 开关(环境变量)

| 变量 | 默认 | 说明 |
|---|---|---|
| `CLAUDE_PROXY_PORT` | 36240 | 监听端口 |
| `CLAUDE_PROXY_UPSTREAM` | api.anthropic.com | 上游 host |
| `CLAUDE_PROXY_PASSTHROUGH` | 0 | =1 纯转发,不进 SSE 拦截(最保守) |
| `CLAUDE_PROXY_SAMPLE` | 1 | =1 脱敏落盘样本 |
| `CLAUDE_PROXY_RESCUE` | 0 | =1 开启救援(阶段 2 实现后) |
| `CLAUDE_PROXY_REDACT` | 1 | =1 落盘前脱敏 token/凭证;=0 样本原文落盘(仅磁盘安全时用) |
| `CLAUDE_PROXY_SAMPLE_KEEP` | 200 | 样本文件保留数(轮转) |

## 日志文件

| 文件 | 内容 | 是否含对话内容 |
|---|---|---|
| `~/.claude-proxy/proxy.log` | 启动信息、上游错误、fail-open 提示 | 否(只有 URL,无请求/响应体) |
| `~/.claude-proxy/audit.log` | 每次救援/退化检测一行 JSON:结果、工具 name、参数**个数**、block index、样本文件名 | 否(**不含参数值**) |
| `~/.claude-proxy/samples/*.sse` | 一次流式响应的完整原始 SSE(默认脱敏) | **是**(含模型输出、工具参数;`CLAUDE_PROXY_REDACT=1` 时凭证已脱敏) |

## 安全

- 只监听 `127.0.0.1`,不暴露网络。
- 落盘前脱敏 `cr_...` token、`Authorization`/`x-api-key`/`Bearer` 头值、body 里的 `token`/`api_key` 字段。**代理内存持有真 token 仅用于转发,绝不写盘。**
- 样本目录 `~/.claude-proxy/samples` 权限 700,自动轮转。

## 文件

| 文件 | 职责 |
|---|---|
| `config.go` | 开关与常量 |
| `proxy.go` | ReverseProxy + ModifyResponse,SSE 分流,透传/救援入口 |
| `sse.go` | SSE 事件解析/序列化 |
| `sample.go` | 脱敏落盘 + 轮转 |
| `redact.go` | 脱敏正则 |
| `rescue.go` | 退化检测与 tool_use 重组(阶段 2 填实现) |
| `analyze.go` | 离线样本分析(`claude-proxy analyze <file>`) |
| `main.go` | 启动、健康检查、子命令分发 |
