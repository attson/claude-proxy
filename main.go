package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version 由 release 构建通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	sub := ""
	if len(os.Args) >= 2 {
		sub = os.Args[1]
	}

	switch sub {
	case "analyze":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: claude-proxy analyze <sample.sse>")
			os.Exit(2)
		}
		analyzeSample(os.Args[2])
		return
	case "run":
		// claude-proxy run [-- <claude args>]:拉起代理并启动 claude 走代理。
		cfg := loadConfig()
		os.Exit(runClaude(cfg, claudeArgsFrom(os.Args[2:])))
	case "serve", "":
		// 对外入口:两层架构的前端 dispatcher(固定端口、永不重启、极薄)。
		// BackendPort 无效(<=0 或 ==Port)时退化为单层 backend,兼容/回退。
		cfg := loadConfig()
		if cfg.BackendPort <= 0 || cfg.BackendPort == cfg.Port {
			serveBackend(cfg)
		} else {
			serveDispatcher(cfg)
		}
		return
	case "backend":
		// 内部 worker:承载真正的转发 + 救援,监听内部端口,上游=真实 api。
		// 由 dispatcher 后台拉起(CLAUDE_PROXY_BACKEND_PORT 指定其监听端口)。
		serveBackend(loadConfig())
		return
	case "version", "--version", "-v":
		fmt.Println("claude-proxy", version)
		return
	case "update":
		checkOnly := false
		for _, a := range os.Args[2:] {
			if a == "--check" {
				checkOnly = true
			}
		}
		os.Exit(selfUpdate(checkOnly))
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\nusage: claude-proxy [serve|run -- <claude args>|analyze <file>]\n", sub)
		os.Exit(2)
	}
}

// claudeArgsFrom 去掉前导的 "--" 分隔符,其余原样透传给 claude。
func claudeArgsFrom(args []string) []string {
	if len(args) > 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

// exitPortInUse 是端口被占用时的退出码,供 run 快速识别"已有代理在跑"。
const exitPortInUse = 3

// serveBackend 启动 backend worker:SSE 反向代理(转发真实上游 + 救援),阻塞监听。
// 两层架构里由 dispatcher 后台拉起(监听内部端口);单层回退时它就是对外代理。
// 除反代外额外暴露本地版本端点 GET /__claude_proxy/version,供 dispatcher 比对版本。
func serveBackend(cfg *Config) {
	initAudit(cfg)

	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.Port)

	// 先抢占端口:成功后才写 pidfile、打 listening 日志,避免"已宣告 listening
	// 却因端口占用秒退"的误导,也避免覆盖别人的 pidfile。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			log.Printf("[proxy] 端口 %s 已被占用(可能已有代理在跑),退出", addr)
			os.Exit(exitPortInUse)
		}
		log.Fatalf("[proxy] 无法监听 %s: %v", addr, err)
	}

	// 端口已拿下:写 pidfile。
	_ = os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)

	// 自指检测:上游若指向代理自己(回环+同端口),转发会死循环,拒绝启动。
	if cfg.isSelfReference() {
		log.Fatalf("[proxy] 上游 %s 指向代理自己,会死循环。请把 ~/.claude/settings.json 的 "+
			"ANTHROPIC_BASE_URL 改为真实上游,或设 CLAUDE_PROXY_UPSTREAM", cfg.Upstream)
	}

	// 健康自检(不阻断启动)。
	if err := healthCheck(cfg); err != nil {
		log.Printf("[proxy] WARNING upstream health check failed: %v (starting anyway)", err)
	} else {
		log.Printf("[proxy] upstream %s reachable", cfg.Upstream)
	}

	proxy := buildProxy(cfg)
	srv := &http.Server{
		Handler:           backendHandler(proxy),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// 优雅退出:收到 SIGTERM(dispatcher 退休旧 backend 时发)→ Shutdown 排空在飞
	// 请求(含 SSE),带超时兜底永不结束的流。Shutdown 后 Serve 返回 ErrServerClosed。
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM)
	go func() {
		<-sigc
		log.Printf("[proxy] 收到 SIGTERM,优雅退出(排空在飞请求)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("[proxy] claude-proxy %s listening on http://%s  ->  %s",
		version, addr, cfg.Upstream)
	log.Printf("[proxy] passthrough_only=%v sample=%v rescue=%v redact=%v",
		cfg.PassthroughOnly, cfg.SampleEnabled, cfg.RescueEnabled, cfg.RedactEnabled)
	if cfg.SampleEnabled && !cfg.RedactEnabled {
		log.Printf("[proxy] WARNING 脱敏已关闭(CLAUDE_PROXY_REDACT=0):样本将含 token 明文,请确保磁盘安全")
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("[proxy] server exited: %v", err)
	}
}

// versionProbePath 是 backend 暴露自身版本的本地专用路径(dispatcher 比对版本用)。
const versionProbePath = "/__claude_proxy/version"

// backendHandler 在反代前拦截本地版本探测路径,其余一律交给反代透传。
// fail-open:只在最前加一个 path 判断,不改反代/转发逻辑。
func backendHandler(proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == versionProbePath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"version":%q}`, version)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// isAddrInUse 判断错误是否为"地址已被占用"。
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) ||
		strings.Contains(err.Error(), "address already in use")
}

// healthCheck 对上游做一次 TCP 连通性探测(不发 HTTP,避免消耗配额)。
func healthCheck(cfg *Config) error {
	addr, err := cfg.upstreamDialAddr()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
