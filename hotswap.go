package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// 版本热切换(阶段二):dispatcher 周期比对"磁盘二进制版本 vs 当前 backend 版本",
// 磁盘更新则起新 backend(新内部端口)、原子替换 current、SIGTERM 退休旧 backend。
// 旧在飞请求粘旧 backend(Director 已按请求绑定 target),旧 backend 排空后自然退出。
// 见 memory two-layer-hot-upgrade。

// hotSwapInterval 是周期自查间隔。取非整点分钟,避免无意义的整点对齐。
const hotSwapInterval = 37 * time.Second

// probeBackendVersion 探某 backend 的版本(GET /__claude_proxy/version);失败返回 ""。
func probeBackendVersion(host string, port int) string {
	url := fmt.Sprintf("http://%s:%d%s", host, port, versionProbePath)
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.Version
}

// diskVersion 读磁盘上当前二进制的版本(exec self version)。
// 升级用 rename 原子替换自身,os.Executable() 路径不变、内容已是新版 → 拿到新版本。失败返回 ""。
func diskVersion() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, self, "version").Output()
	if err != nil {
		return ""
	}
	// 输出形如 "claude-proxy vX.Y.Z"
	fields := splitFields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// splitFields 是极简空白分词(避免引 strings 只为一次 Fields)。
func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// hotSwapMu 串行化热切换,避免周期 tick 与并发触发重入。
var hotSwapMu sync.Mutex

// runHotSwapLoop 周期自查:磁盘版本比当前 backend 新则切换。dispatcher 后台常驻调用。
func runHotSwapLoop(cfg *Config, current *atomic.Pointer[backendState]) {
	for {
		time.Sleep(hotSwapInterval)
		maybeHotSwap(cfg, current)
	}
}

// maybeHotSwap 执行一次版本比对与(必要时)切换。全程 fail-open:任何异常都不影响现有转发。
func maybeHotSwap(cfg *Config, current *atomic.Pointer[backendState]) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[dispatcher] hotswap panic (fail-open): %v", r)
		}
	}()

	cur := current.Load()
	if cur == nil {
		return
	}
	disk := diskVersion()
	if disk == "" {
		return // 读不到磁盘版本,保守不动
	}
	// 用当前 backend 已探得的版本;为空则现探一次。
	curVer := cur.version
	if curVer == "" {
		curVer = probeBackendVersion(cfg.ListenHost, cur.port)
	}
	if !isNewer(disk, curVer) {
		return // 磁盘不比当前新,无需切换
	}

	hotSwapMu.Lock()
	defer hotSwapMu.Unlock()
	// 双检:可能已被上一次切换处理。
	if c := current.Load(); c != nil && !isNewer(disk, c.version) {
		return
	}

	log.Printf("[dispatcher] 检测到磁盘新版 %s(当前 backend %s),起新 backend 切换...", disk, curVer)

	newPort := freeBackendPort(cfg, cur.port)
	if newPort == 0 {
		log.Printf("[dispatcher] 找不到空闲端口起新 backend,放弃本次切换")
		return
	}
	ns, err := startBackendOn(cfg, newPort)
	if err != nil {
		log.Printf("[dispatcher] 起新 backend 失败(fail-open,继续用旧的): %v", err)
		return
	}
	// 新 backend 版本必须确实 >= 磁盘目标,否则可能起了个不对的,回退不切、退休它。
	if ns.version != "" && isNewer(disk, ns.version) {
		log.Printf("[dispatcher] 新 backend 版本 %s 仍旧于磁盘 %s,放弃切换并退休它", ns.version, disk)
		retireBackend(ns)
		return
	}

	old := current.Load()
	current.Store(ns) // 之后进 Director 的新请求走新 backend
	log.Printf("[dispatcher] 已切换到新 backend %s(版本 %s),退休旧 backend %s", ns.target, ns.version, old.target)

	// 退休旧 backend:发 SIGTERM,它 Shutdown 排空在飞请求后退出。旧在飞请求已粘旧 backend,不受影响。
	if old != nil && old.pid > 0 {
		retireBackend(old)
	}
}

// freeBackendPort 从 base+1 起找一个未监听的回环端口(排除 avoid)。找不到返回 0。
func freeBackendPort(cfg *Config, avoid int) int {
	base := cfg.BackendPort
	for p := base + 1; p <= base+50; p++ {
		if p == avoid || p == cfg.Port {
			continue
		}
		if !portListening(cfg.ListenHost, p) {
			return p
		}
	}
	return 0
}

// retireBackend 退休一个 backend 进程:Unix 发 SIGTERM(其 signal handler 走 srv.Shutdown
// 优雅排空后退出);Windows 无优雅信号,降级为 Kill(见 spawn_*.go 的 terminateProcess)。
func retireBackend(bs *backendState) {
	if bs == nil || bs.pid <= 0 {
		return
	}
	if err := terminateProcess(bs.pid); err != nil {
		log.Printf("[dispatcher] 退休旧 backend pid=%d 失败: %v", bs.pid, err)
	}
}
