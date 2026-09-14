package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

	// 0. 确定真实上游:优先显式 CLAUDE_PROXY_UPSTREAM,否则读 ~/.claude/settings.json
	//    的 env.ANTHROPIC_BASE_URL,否则用 cfg 默认。写回 cfg 供 spawnProxy 透传。
	if os.Getenv("CLAUDE_PROXY_UPSTREAM") == "" {
		if up := readSettingsBaseURL(); up != "" {
			cfg.Upstream = up
		}
	}
	// 自指前置检查:避免后台 serve 起来又 fatal 退出、run 侧只看到超时。
	if cfg.isSelfReference() {
		fmt.Fprintf(os.Stderr, "[run] 上游 %s 指向代理自己,会死循环。请把 ~/.claude/settings.json 的 "+
			"ANTHROPIC_BASE_URL 改为真实上游(如 https://api.anthropic.com)\n", cfg.Upstream)
		return 1
	}

	// 1. 确保代理在跑
	if err := ensureProxy(cfg, baseURL); err != nil {
		fmt.Fprintf(os.Stderr, "[run] %v\n", err)
		return 1
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

// ensureProxy 确保本地代理在监听:已在跑则复用;否则后台拉起并等待就绪。
// 处理竞态:若拉起的进程因端口被抢占而秒退,则说明已有别的代理在跑,复用之。
func ensureProxy(cfg *Config, baseURL string) error {
	if proxyListening(cfg) {
		fmt.Fprintf(os.Stderr, "[run] 复用已在运行的代理 %s\n", baseURL)
		return nil
	}

	fmt.Fprintf(os.Stderr, "[run] 代理未运行,后台拉起 %s ...\n", baseURL)
	pid, err := spawnProxy(cfg)
	if err != nil {
		return fmt.Errorf("拉起代理失败: %v", err)
	}

	// 轮询就绪:端口起来了 => 成功;子进程已消失且端口仍不通 => 秒退失败。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if proxyListening(cfg) {
			fmt.Fprintf(os.Stderr, "[run] 代理已就绪\n")
			return nil
		}
		if !pidAlive(pid) {
			// 子进程退出了。可能是端口被别人抢占(秒退)——若此刻端口通,复用;
			// 否则给 100ms 让端口真正 up 后再判失败。
			time.Sleep(150 * time.Millisecond)
			if proxyListening(cfg) {
				fmt.Fprintf(os.Stderr, "[run] 复用已在运行的代理 %s\n", baseURL)
				return nil
			}
			return fmt.Errorf("代理启动后立即退出(可能端口 %d 被非代理进程占用),看 %s", cfg.Port, cfg.LogFile)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("代理未能在 5s 内就绪,看 %s", cfg.LogFile)
}

// spawnProxy 后台拉起一个自身作代理进程(serve 模式,开启救援),返回其 pid。
func spawnProxy(cfg *Config) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	proc := exec.Command(self, "serve")
	// 把 run 确定的上游透传给后台 serve(它会经 loadConfig 的 CLAUDE_PROXY_UPSTREAM 读到)。
	proc.Env = append(os.Environ(), "CLAUDE_PROXY_RESCUE=1", "CLAUDE_PROXY_UPSTREAM="+cfg.Upstream)
	proc.Stdout = logf
	proc.Stderr = logf
	proc.Stdin = nil
	// 脱离父进程会话,后台常驻(平台相关实现见 spawn_*.go)
	proc.SysProcAttr = detachSysProcAttr()
	if err := proc.Start(); err != nil {
		_ = logf.Close()
		return 0, err
	}
	pid := proc.Process.Pid
	// 注意:不写 pidfile —— pidfile 由 serve 抢到端口后自己写,避免"秒退进程
	// 覆盖了在跑代理的 pidfile"。这里只 Release 让它脱离父进程。
	_ = proc.Process.Release()
	return pid, nil
}

// readSettingsBaseURL 读 ~/.claude/settings.json 的 env.ANTHROPIC_BASE_URL。
// 文件缺失/无该键/解析失败一律返回 ""(由调用方回退默认)。不 log 文件内容(含 token)。
func readSettingsBaseURL() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(data, &s) != nil {
		return ""
	}
	return s.Env["ANTHROPIC_BASE_URL"]
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
