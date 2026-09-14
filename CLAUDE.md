# claude-proxy

本机 SSE 反向代理,修复 Claude Code CLI 偶发的工具调用退化 bug。详见 README.md。

## 核心原则

- **fail-open**:任何转发/改写逻辑的异常一律原样透传,绝不破坏正常请求。改 `proxy.go` 时必须守住这条。

## 构建 / 测试

- 需要 **Go 1.23+**(`go.mod` 要求 1.23)。系统 `go` 版本过低会报 `cannot compile Go 1.23 code`。
- 提交前跑:`go vet ./... && go build -o claude-proxy . && go test ./...`

## 发版

- push `v*` tag 触发 `.github/workflows/release.yml`,自动跨平台构建 6 个产物 + 建 GitHub Release。
- 流程:分支 → PR(CI 跑 vet/test/6 平台交叉构建)→ squash 合并 main → `git tag -a vX.Y.Z -m ... && git push origin vX.Y.Z`。纯 bugfix 用 patch 版本号。

## 排查

- proxy 运行日志:`~/.claude-proxy/proxy.log`,`grep "upstream error"`。
- 区分:`context canceled` = CLI 侧正常取消(非 bug);`cannot retry ... GetBody` / `502` = 本项目问题。
- 隔离验证:换 `CLAUDE_PROXY_PORT` 起临时实例做冒烟测试,不碰在跑的活代理。pidfile 按端口命名 `proxy-<port>.pid`。
