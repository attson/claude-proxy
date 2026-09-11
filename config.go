package main

import (
	"os"
	"path/filepath"
	"strconv"
)

// 配置集中于此。多数项可用环境变量覆盖,改完重启代理生效。
type Config struct {
	ListenHost string
	Port       int

	// 真实上游(Claude Code 原本直连的网关)。回退时把 settings.json 的
	// ANTHROPIC_BASE_URL 改回 https://<UpstreamHost>/ 即彻底绕开代理。
	UpstreamScheme string
	UpstreamHost   string

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
	return &Config{
		ListenHost:      "127.0.0.1",
		Port:            envInt("CLAUDE_PROXY_PORT", 36240),
		UpstreamScheme:  "https",
		UpstreamHost:    envStr("CLAUDE_PROXY_UPSTREAM", "api.anthropic.com"),
		PassthroughOnly: envBool("CLAUDE_PROXY_PASSTHROUGH", false),
		SampleEnabled:   envBool("CLAUDE_PROXY_SAMPLE", true),
		RescueEnabled:   envBool("CLAUDE_PROXY_RESCUE", false),
		RedactEnabled:   envBool("CLAUDE_PROXY_REDACT", true),
		SampleDir:       filepath.Join(base, "samples"),
		SampleKeep:      envInt("CLAUDE_PROXY_SAMPLE_KEEP", 200),
		LogFile:         filepath.Join(base, "proxy.log"),
		PidFile:         filepath.Join(base, "proxy.pid"),
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
