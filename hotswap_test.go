package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// mustListen 占住一个 TCP 地址,测试辅助。
func mustListen(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("无法监听 %s(端口可能被占,跳过): %v", addr, err)
	}
	return ln
}

// TestProbeBackendVersion:能从 backend 版本端点解析出版本;不可达返回空。
func TestProbeBackendVersion(t *testing.T) {
	srv := httptest.NewServer(backendHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	host := u.Hostname()
	var port int
	fmt.Sscanf(u.Port(), "%d", &port)

	if got := probeBackendVersion(host, port); got != version {
		t.Errorf("probeBackendVersion = %q, want %q", got, version)
	}
	// 不可达端口 → 空。
	if got := probeBackendVersion("127.0.0.1", 1); got != "" {
		t.Errorf("不可达应返回空,得到 %q", got)
	}
}

// TestSplitFields:版本输出分词取最后一段。
func TestSplitFields(t *testing.T) {
	cases := map[string]string{
		"claude-proxy v1.2.3\n": "v1.2.3",
		"  claude-proxy   dev ": "dev",
		"single":                "single",
		"":                      "",
	}
	for in, want := range cases {
		f := splitFields(in)
		got := ""
		if len(f) > 0 {
			got = f[len(f)-1]
		}
		if got != want {
			t.Errorf("splitFields(%q) last = %q, want %q", in, got, want)
		}
	}
}

// TestFreeBackendPort:跳过 avoid 与对外 Port,返回未监听端口。
func TestFreeBackendPort(t *testing.T) {
	cfg := &Config{ListenHost: "127.0.0.1", Port: 46000, BackendPort: 47000}
	// 占住 47001,验证会跳过它。
	ln := mustListen(t, "127.0.0.1:47001")
	defer ln.Close()

	p := freeBackendPort(cfg, cfg.BackendPort)
	if p == 0 {
		t.Fatal("应找到空闲端口")
	}
	if p == cfg.BackendPort || p == 47001 || p == cfg.Port {
		t.Errorf("返回的端口 %d 不该是被占/避开的", p)
	}
	if !(p > cfg.BackendPort) {
		t.Errorf("端口应 > BackendPort,得到 %d", p)
	}
}

// TestMaybeHotSwapNoop:磁盘版本不比当前新时,current 不变(不切换)。
// 这里通过让 backend 版本 == 磁盘版本(都是 version)来验证 noop 分支。
func TestMaybeHotSwapNoop(t *testing.T) {
	// 起一个真 backend(版本端点返回当前 version),current 指向它。
	be := httptest.NewServer(backendHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer be.Close()
	u, _ := url.Parse(be.URL)
	var port int
	fmt.Sscanf(u.Port(), "%d", &port)

	cfg := &Config{ListenHost: "127.0.0.1", Port: 46000, BackendPort: port, Upstream: "http://127.0.0.1:1"}
	var current atomic.Pointer[backendState]
	target, _ := url.Parse(be.URL)
	current.Store(&backendState{target: target, pid: 0, port: port, version: version})

	before := current.Load()
	maybeHotSwap(cfg, &current) // diskVersion()==version==backend version → isNewer false → noop
	after := current.Load()
	if before != after {
		t.Errorf("版本相同不应切换 backend")
	}
}

// TestBackendHandlerVersionMatchesBinary:版本端点返回的正是 main.version。
func TestBackendHandlerVersionMatchesBinary(t *testing.T) {
	rec := httptest.NewRecorder()
	backendHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", versionProbePath, nil))
	if !strings.Contains(rec.Body.String(), version) {
		t.Errorf("版本端点未含二进制版本 %q: %s", version, rec.Body.String())
	}
}
