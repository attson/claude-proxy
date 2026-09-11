package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
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

// serve 启动 SSE 反向代理并阻塞监听。
func serve(cfg *Config) {
	initAudit(cfg)

	// 写 pidfile(供 stop.sh 用)。
	_ = os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)

	// 启动前健康自检:对上游做一次连通性探测。
	if err := healthCheck(cfg); err != nil {
		log.Printf("[proxy] WARNING upstream health check failed: %v (starting anyway)", err)
	} else {
		log.Printf("[proxy] upstream %s reachable", cfg.UpstreamHost)
	}

	proxy := buildProxy(cfg)
	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: proxy,
		// 流式长连接:不设写超时,读头超时给足。
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Printf("[proxy] claude-proxy %s listening on http://%s  ->  %s://%s",
		version, addr, cfg.UpstreamScheme, cfg.UpstreamHost)
	log.Printf("[proxy] passthrough_only=%v sample=%v rescue=%v redact=%v",
		cfg.PassthroughOnly, cfg.SampleEnabled, cfg.RescueEnabled, cfg.RedactEnabled)
	if cfg.SampleEnabled && !cfg.RedactEnabled {
		log.Printf("[proxy] WARNING 脱敏已关闭(CLAUDE_PROXY_REDACT=0):样本将含 token 明文,请确保磁盘安全")
	}
	log.Printf("[proxy] 接入: 把 ~/.claude/settings.json 的 ANTHROPIC_BASE_URL 改为 http://%s 并重启 Claude Code", addr)

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[proxy] server exited: %v", err)
	}
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
