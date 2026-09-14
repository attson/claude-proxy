package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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
		serve(loadConfig())
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

// serve 启动 SSE 反向代理并阻塞监听。
func serve(cfg *Config) {
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

	// 健康自检(不阻断启动)。
	if err := healthCheck(cfg); err != nil {
		log.Printf("[proxy] WARNING upstream health check failed: %v (starting anyway)", err)
	} else {
		log.Printf("[proxy] upstream %s reachable", cfg.UpstreamHost)
	}

	proxy := buildProxy(cfg)
	srv := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Printf("[proxy] claude-proxy %s listening on http://%s  ->  %s://%s",
		version, addr, cfg.UpstreamScheme, cfg.UpstreamHost)
	log.Printf("[proxy] passthrough_only=%v sample=%v rescue=%v redact=%v",
		cfg.PassthroughOnly, cfg.SampleEnabled, cfg.RescueEnabled, cfg.RedactEnabled)
	if cfg.SampleEnabled && !cfg.RedactEnabled {
		log.Printf("[proxy] WARNING 脱敏已关闭(CLAUDE_PROXY_REDACT=0):样本将含 token 明文,请确保磁盘安全")
	}

	if err := srv.Serve(ln); err != nil {
		log.Fatalf("[proxy] server exited: %v", err)
	}
}

// isAddrInUse 判断错误是否为"地址已被占用"。
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE) ||
		strings.Contains(err.Error(), "address already in use")
}

// healthCheck 对上游 443 做一次 TCP 连通性探测(不发 HTTP,避免消耗配额)。
func healthCheck(cfg *Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", cfg.UpstreamHost+":443")
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
