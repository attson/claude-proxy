package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mustURL 解析 URL,测试辅助。
func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url %q: %v", s, err)
	}
	return u
}

// TestBackendHandlerVersionProbe:版本探测路径返回版本 JSON,其它路径进 proxy。
func TestBackendHandlerVersionProbe(t *testing.T) {
	hitProxy := false
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitProxy = true
		w.WriteHeader(200)
	})
	h := backendHandler(proxy)

	// 版本路径:不进 proxy,返回含 version 的 JSON。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", versionProbePath, nil))
	if hitProxy {
		t.Errorf("版本探测路径不应进 proxy")
	}
	if !strings.Contains(rec.Body.String(), `"version"`) {
		t.Errorf("版本端点未返回 version 字段: %s", rec.Body.String())
	}

	// 其它路径:进 proxy。
	hitProxy = false
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/messages", nil))
	if !hitProxy {
		t.Errorf("普通路径应进 proxy")
	}
}

// TestDispatchProxySSENotBuffered:dispatcher 反代穿 SSE,逐 chunk 及时到达(不被缓冲成一坨)。
func TestDispatchProxySSENotBuffered(t *testing.T) {
	const gap = 150 * time.Millisecond
	// 假 backend:逐 chunk flush 的 SSE。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(gap)
		}
	}))
	defer backend.Close()

	cfg := &Config{ListenHost: "127.0.0.1", Upstream: "http://127.0.0.1:1"} // 上游随便(不逃生)
	var current atomic.Pointer[backendState]
	current.Store(&backendState{target: mustURL(t, backend.URL)})
	disp := httptest.NewServer(newDispatchProxy(cfg, &current))
	defer disp.Close()

	resp, err := http.Get(disp.URL + "/")
	if err != nil {
		t.Fatalf("GET dispatcher: %v", err)
	}
	defer resp.Body.Close()

	// 逐行读,记录每个 chunk 到达的时刻;相邻 chunk 间隔应接近 gap(未被整流)。
	sc := bufio.NewScanner(resp.Body)
	var times []time.Time
	for sc.Scan() {
		if strings.Contains(sc.Text(), "chunk-") {
			times = append(times, time.Now())
		}
	}
	if len(times) != 3 {
		t.Fatalf("期望 3 个 chunk,得到 %d", len(times))
	}
	// 若被缓冲成一坨,三个几乎同时到(间隔≈0)。要求相邻间隔至少 gap 的一半。
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < gap/2 {
			t.Errorf("chunk %d 与 %d 间隔 %v 过小,疑似被缓冲", i-1, i, d)
		}
	}
}

// TestDispatchProxyFailOpenEscape:backend 不可达时,dispatcher 逃生直连真实上游。
func TestDispatchProxyFailOpenEscape(t *testing.T) {
	// 真实上游(逃生目标):返回可识别标记。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ESCAPED-TO-UPSTREAM")
	}))
	defer upstream.Close()

	cfg := &Config{ListenHost: "127.0.0.1", Upstream: upstream.URL}
	var current atomic.Pointer[backendState]
	// current 指向一个不可达的 backend(端口 1,必失败)→ 触发 ErrorHandler 逃生。
	current.Store(&backendState{target: mustURL(t, "http://127.0.0.1:1")})
	disp := httptest.NewServer(newDispatchProxy(cfg, &current))
	defer disp.Close()

	resp, err := http.Get(disp.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("GET dispatcher: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ESCAPED-TO-UPSTREAM") {
		t.Errorf("backend 不可达时未逃生到真实上游,得到: %q (status %d)", body, resp.StatusCode)
	}
}

// TestDispatchProxyRoutesToCurrent:请求路由到 current 指向的 backend;Store 后新请求走新 backend。
func TestDispatchProxyRoutesToCurrent(t *testing.T) {
	mk := func(tag string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, tag)
		}))
	}
	b1 := mk("BACKEND-1")
	defer b1.Close()
	b2 := mk("BACKEND-2")
	defer b2.Close()

	cfg := &Config{ListenHost: "127.0.0.1", Upstream: "http://127.0.0.1:1"}
	var current atomic.Pointer[backendState]
	current.Store(&backendState{target: mustURL(t, b1.URL)})
	disp := httptest.NewServer(newDispatchProxy(cfg, &current))
	defer disp.Close()

	get := func() string {
		resp, err := http.Get(disp.URL + "/")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if got := get(); got != "BACKEND-1" {
		t.Errorf("初始应路由到 b1,得到 %q", got)
	}
	// 原子替换 current → 新请求走 b2。
	current.Store(&backendState{target: mustURL(t, b2.URL)})
	if got := get(); got != "BACKEND-2" {
		t.Errorf("Store 后应路由到 b2,得到 %q", got)
	}
}
