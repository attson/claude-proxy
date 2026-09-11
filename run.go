package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"
)

// runClaude 实现 `claude-proxy run [-- <claude args>]`:
//  1. 确保本地代理在监听(不在则后台拉起一个,开启救援);
//  2. 组装 env(ANTHROPIC_BASE_URL 指向代理 + NO_PROXY + 去嵌套污染);
//  3. 用 --settings 内联 JSON 强制覆盖 settings.json 里的 base_url;
//  4. 前台启动 claude,转发信号,等其退出并透传退出码。
//
// 参考 claude-tap 的 reverse 模式:明文 HTTP 反代 + --settings 优先级绕过。
func runClaude(cfg *Config, claudeArgs []string) int {
	baseURL := fmt.Sprintf("http://%s:%d", cfg.ListenHost, cfg.Port)

	// 1. 确保代理在跑
	if !proxyListening(cfg) {
		fmt.Fprintf(os.Stderr, "[run] 代理未运行,后台拉起 %s ...\n", baseURL)
		if err := spawnProxy(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "[run] 拉起代理失败: %v\n", err)
			return 1
		}
		if !waitListening(cfg, 5*time.Second) {
			fmt.Fprintf(os.Stderr, "[run] 代理未能就绪,看 %s\n", cfg.LogFile)
			return 1
		}
		fmt.Fprintf(os.Stderr, "[run] 代理已就绪\n")
	} else {
		fmt.Fprintf(os.Stderr, "[run] 复用已在运行的代理 %s\n", baseURL)
	}

	// 2. 组装 env
	env := os.Environ()
	env = setEnv(env, "ANTHROPIC_BASE_URL", baseURL)
	env = setEnv(env, "NO_PROXY", cfg.ListenHost)
	env = delEnv(env, "CLAUDECODE") // 防 claude 嵌套状态污染
	env = delEnv(env, "CLAUDE_CODE_SSE_PORT")

	// 3. --settings 内联 JSON(优先级高于 ~/.claude/settings.json),
	//    仅当用户没自带 --settings 时注入,尊重用户显式配置。
	args := claudeArgs
	if !hasSettingsArg(claudeArgs) {
		payload, _ := json.Marshal(map[string]any{
			"env": map[string]string{"ANTHROPIC_BASE_URL": baseURL},
		})
		args = append([]string{"--settings", string(payload)}, claudeArgs...)
	}

	// 4. 启动 claude
	claudeBin := envStr("CLAUDE_PROXY_CLAUDE_BIN", "claude")
	cmd := exec.Command(claudeBin, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[run] 启动 %s 失败: %v\n", claudeBin, err)
		return 1
	}

	// 转发信号给子进程(Ctrl-C 等)。信号集平台相关,见 spawn_*.go。
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, forwardedSignals()...)
	go func() {
		for s := range sigc {
			_ = cmd.Process.Signal(s)
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigc)
	close(sigc)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "[run] claude 退出异常: %v\n", err)
		return 1
	}
	return 0
}

// proxyListening 探测代理端口是否已在监听。
func proxyListening(cfg *Config) bool {
	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.Port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitListening(cfg *Config, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxyListening(cfg) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// spawnProxy 后台拉起一个自身作代理进程(serve 模式,开启救援)。
func spawnProxy(cfg *Config) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	proc := exec.Command(self, "serve")
	proc.Env = append(os.Environ(), "CLAUDE_PROXY_RESCUE=1")
	proc.Stdout = logf
	proc.Stderr = logf
	proc.Stdin = nil
	// 脱离父进程会话,后台常驻(平台相关实现见 spawn_*.go)
	proc.SysProcAttr = detachSysProcAttr()
	if err := proc.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	_ = os.WriteFile(cfg.PidFile, []byte(fmt.Sprintf("%d", proc.Process.Pid)), 0o600)
	_ = proc.Process.Release()
	return nil
}

func hasSettingsArg(args []string) bool {
	for _, a := range args {
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			return true
		}
	}
	return false
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := env[:0:0]
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, prefix+val)
			found = true
		} else {
			out = append(out, e)
		}
	}
	if !found {
		out = append(out, prefix+val)
	}
	return out
}

func delEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}
