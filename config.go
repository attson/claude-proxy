package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 配置集中于此。多数项可用环境变量覆盖,改完重启代理生效。
type Config struct {
	ListenHost string
	Port       int

	// 两层架构:dispatcher 监听对外 Port(默认 36240),把请求转发给 backend
	// worker 监听的内部端口 BackendPort(默认 Port+1000,回环)。BackendPort<=0
	// 或与 Port 相同则退化为单层(不启用两层,兼容/回退)。
	// backend 进程由 dispatcher 用 env CLAUDE_PROXY_BACKEND_PORT 指定其监听端口。
	BackendPort int

	// 真实上游 base URL(完整,含 scheme,可含端口/路径前缀)。
	// 来源优先级:环境变量 CLAUDE_PROXY_UPSTREAM > run 从 ~/.claude/settings.json
	// 读到的 ANTHROPIC_BASE_URL > 默认 https://api.anthropic.com。
	// run 子命令读 settings 后经 CLAUDE_PROXY_UPSTREAM 传给后台 serve 进程。
	Upstream string

	// 阶段开关:
	//   PassthroughOnly=true  纯转发,不进 SSE 拦截(最保守,回退用)。
	//   SampleEnabled=true    对流式响应脱敏落盘原始 SSE(阶段一抓样本)。
	//   RescueEnabled=false   阶段二据样本填 rescue.go 后再开。
	PassthroughOnly bool
	SampleEnabled   bool
	RescueEnabled   bool

	// 脱敏开关:默认 true(落盘前脱敏 token/凭证)。设为 false 则样本原文落盘
	// (仅在你确认磁盘安全、需要看未脱敏原文排查时使用)。
	RedactEnabled bool

	SampleDir  string
	SampleKeep int
	LogFile    string
	PidFile    string
}

func loadConfig() *Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".claude-proxy")
	port := envInt("CLAUDE_PROXY_PORT", 36240)
	return &Config{
		ListenHost:      "127.0.0.1",
		Port:            port,
		BackendPort:     envInt("CLAUDE_PROXY_BACKEND_PORT", port+1000),
		Upstream:        envStr("CLAUDE_PROXY_UPSTREAM", "https://api.anthropic.com"),
		PassthroughOnly: envBool("CLAUDE_PROXY_PASSTHROUGH", false),
		SampleEnabled:   envBool("CLAUDE_PROXY_SAMPLE", true),
		RescueEnabled:   envBool("CLAUDE_PROXY_RESCUE", false),
		RedactEnabled:   envBool("CLAUDE_PROXY_REDACT", true),
		SampleDir:       filepath.Join(base, "samples"),
		SampleKeep:      envInt("CLAUDE_PROXY_SAMPLE_KEEP", 200),
		LogFile:         filepath.Join(base, "proxy.log"),
		// pidfile 带端口:不同端口的实例各写各的,避免换端口起的实例(如冒烟
		// 测试)覆盖掉默认实例的 pidfile,进而害得 stop 杀错/杀空。
		PidFile: filepath.Join(base, fmt.Sprintf("proxy-%d.pid", port)),
	}
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		return v == "1" || v == "true"
	}
	return def
}

// upstreamURL 解析上游 base URL。路径尾斜杠归一化(去掉),避免转发拼出 //v1/...。
func (c *Config) upstreamURL() (*url.URL, error) {
	u, err := url.Parse(c.Upstream)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// upstreamDialAddr 返回用于 TCP 健康探测的 host:port(按 scheme 补默认端口)。
func (c *Config) upstreamDialAddr() (string, error) {
	u, err := c.upstreamURL()
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// isSelfReference 判断上游是否指向代理自己(回环地址 + 同端口),防死循环。
func (c *Config) isSelfReference() bool {
	u, err := c.upstreamURL()
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	port := u.Port()
	if port == "" {
		return false // 回环但无端口,不太可能是本代理
	}
	return port == strconv.Itoa(c.Port)
}
