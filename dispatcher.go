package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

// dispatcher 是两层架构的前端:固定监听对外端口(如 36240)、永不重启、极薄。
// 它只做一件事——把每个请求转发给"当前 backend worker"(内部回环端口)。
// backend 承载真正会随版本迭代的转发/救援逻辑;dispatcher 无状态、几乎不需升级。
//
// 阶段一:current 固定为启动时拉起的单个 backend(不热切换)。
// 阶段二:磁盘版本更新时起新 backend、原子替换 current、SIGTERM 退休旧 backend。

// backendState 保存一个 backend worker 的地址、pid、监听端口与版本。
type backendState struct {
	target  *url.URL
	pid     int
	port    int
	version string // 该 backend 进程的 claude-proxy 版本(经 /__claude_proxy/version 探得)
}

// serveDispatcher 启动 dispatcher:抢对外端口、确保 backend 在跑、转发。
func serveDispatcher(cfg *Config) {
	addr := fmt.Sprintf("%s:%d", cfg.ListenHost, cfg.Port)

	// 先抢占对外端口:成功后才写 pidfile,避免误导与覆盖别人 pidfile。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			log.Printf("[dispatcher] 端口 %s 已被占用(可能已有 dispatcher 在跑),退出", addr)
			os.Exit(exitPortInUse)
		}
		log.Fatalf("[dispatcher] 无法监听 %s: %v", addr, err)
	}
	_ = os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)

	// 自指检测:真实上游若指向 dispatcher 对外端口,会死循环。
	if cfg.isSelfReference() {
		log.Fatalf("[dispatcher] 真实上游 %s 指向 dispatcher 自己,会死循环。"+
			"请把 ~/.claude/settings.json 的 ANTHROPIC_BASE_URL 改为真实上游,或设 CLAUDE_PROXY_UPSTREAM", cfg.Upstream)
	}

	// 确保 backend 在跑,拿到其 target。
	var current atomic.Pointer[backendState]
	bs, err := ensureBackend(cfg)
	if err != nil {
		log.Fatalf("[dispatcher] 无法拉起 backend: %v", err)
	}
	current.Store(bs)

	proxy := newDispatchProxy(cfg, &current)
	srv := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Printf("[dispatcher] claude-proxy %s listening on http://%s  ->  backend %s (%s) (real upstream %s)",
		version, addr, bs.target, bs.version, cfg.Upstream)

	// 阶段二:后台周期自查磁盘版本,新则起新 backend 热切换、退休旧 backend。
	go runHotSwapLoop(cfg, &current)

	if err := srv.Serve(ln); err != nil {
		log.Fatalf("[dispatcher] server exited: %v", err)
	}
}

// newDispatchProxy 构造 dispatcher 的反代:哑转发到 current backend。
// 关键(见 memory two-layer-hot-upgrade):
//   - FlushInterval:-1 保证 SSE 逐 chunk 不缓冲(两层各自都要设)。
//   - 不挂 ModifyResponse(不二次救援);Director 不动 Accept-Encoding(那是 backend 对真实上游那跳的职责)。
//   - Director 每请求读一次 current,整条请求(含 SSE 流)绑定该 backend → 阶段二热切换时旧在飞请求粘旧 backend。
//   - ErrorHandler fail-open:连不上 backend 时兜底直连真实上游(等价单层 passthrough),不因多一层引入新失败面。
func newDispatchProxy(cfg *Config, current *atomic.Pointer[backendState]) *httputil.ReverseProxy {
	realUpstream, _ := cfg.upstreamURL() // 逃生用;解析失败则 nil,ErrorHandler 里再判

	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			bs := current.Load()
			if bs == nil || bs.target == nil {
				return // ErrorHandler 会兜底
			}
			req.URL.Scheme = bs.target.Scheme
			req.URL.Host = bs.target.Host
			req.Host = bs.target.Host
			// 注意:不改 Accept-Encoding —— 压缩降级/解压只在 backend 对真实上游那跳做。
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// backend 连不上 → fail-open 逃生:直连真实上游。
			if realUpstream != nil && escapeToUpstream(realUpstream, w, r) {
				return
			}
			log.Printf("[dispatcher] backend error 且逃生失败: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":502,"message":"dispatcher: backend unavailable"}`))
		},
	}
	return rp
}

// escapeToUpstream 是 dispatcher 的逃生通道:backend 挂掉时,把请求直接反代到真实上游。
// 尽力而为;成功发起返回 true。注意 body 可能已被读过(Director 阶段未消费,通常仍可用)。
func escapeToUpstream(upstream *url.URL, w http.ResponseWriter, r *http.Request) bool {
	esc := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			if upstream.Path != "" {
				req.URL.Path = singleJoiningSlash(upstream.Path, req.URL.Path)
			}
			req.Header.Set("Accept-Encoding", "identity")
		},
	}
	defer func() { _ = recover() }() // 逃生本身也 fail-open,绝不 panic
	esc.ServeHTTP(w, r)
	return true
}

// ensureBackend 确保 cfg.BackendPort 上有 backend worker:不在则拉起并等就绪。
// 启动时用;热切换起新 backend 走 startBackendOn(新端口)。
func ensureBackend(cfg *Config) (*backendState, error) {
	if portListening(cfg.ListenHost, cfg.BackendPort) {
		// 复用已在跑的 backend(探其版本)。
		target, _ := url.Parse(fmt.Sprintf("http://%s:%d", cfg.ListenHost, cfg.BackendPort))
		return &backendState{target: target, pid: 0, port: cfg.BackendPort,
			version: probeBackendVersion(cfg.ListenHost, cfg.BackendPort)}, nil
	}
	return startBackendOn(cfg, cfg.BackendPort)
}

// startBackendOn 在指定端口拉起一个新 backend 并等就绪,返回其 state(含探得的版本)。
func startBackendOn(cfg *Config, port int) (*backendState, error) {
	target, err := url.Parse(fmt.Sprintf("http://%s:%d", cfg.ListenHost, port))
	if err != nil {
		return nil, err
	}
	pid, err := spawnBackend(cfg, port)
	if err != nil {
		return nil, err
	}
	ready := func() *backendState {
		return &backendState{target: target, pid: pid, port: port,
			version: probeBackendVersion(cfg.ListenHost, port)}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if portListening(cfg.ListenHost, port) {
			return ready(), nil
		}
		if !pidAlive(pid) {
			time.Sleep(150 * time.Millisecond)
			if portListening(cfg.ListenHost, port) {
				return ready(), nil
			}
			return nil, fmt.Errorf("backend 启动后立即退出(端口 %d),看 %s", port, cfg.LogFile)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("backend 未能在 5s 内就绪(端口 %d),看 %s", port, cfg.LogFile)
}

// portListening 探测某回环端口是否在监听。
func portListening(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// spawnBackend 后台拉起 backend 子进程(自身 backend 子命令,监听内部端口)。
// 复用 detach 机制(spawn_*.go)。backend 用 CLAUDE_PROXY_PORT=内部端口 监听,
// CLAUDE_PROXY_BACKEND_PORT=0 使其自身为单层(不再往下拉)。真实上游经 env 透传。
func spawnBackend(cfg *Config, port int) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	proc := exec.Command(self, "backend")
	proc.Env = append(os.Environ(),
		"CLAUDE_PROXY_RESCUE=1",
		"CLAUDE_PROXY_UPSTREAM="+cfg.Upstream,
		"CLAUDE_PROXY_PORT="+strconv.Itoa(port),
		"CLAUDE_PROXY_BACKEND_PORT=0",
	)
	proc.Stdout = logf
	proc.Stderr = logf
	proc.Stdin = nil
	proc.SysProcAttr = detachSysProcAttr()
	if err := proc.Start(); err != nil {
		_ = logf.Close()
		return 0, err
	}
	pid := proc.Process.Pid
	_ = proc.Process.Release()
	return pid, nil
}
