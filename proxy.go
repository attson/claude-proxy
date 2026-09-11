package main

import (
	"bufio"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// degradeSniffer 在透传时嗅探退化信号,不改写流。只保留一个有上限的滑动
// 窗口(退化标记 count/court<invoke 很短,窗口足够),避免缓冲整流。
type degradeSniffer struct {
	tail []byte
	hit  bool
}

func (d *degradeSniffer) feed(chunk []byte) {
	if d.hit {
		return
	}
	const window = 4096
	d.tail = append(d.tail, chunk...)
	if len(d.tail) > window {
		d.tail = d.tail[len(d.tail)-window:]
	}
	// 流字节里 <invoke name= 不受 JSON \n 转义影响,可直接匹配作粗略提示。
	if sniffInvoke.Match(d.tail) {
		d.hit = true
	}
}

var sniffInvoke = regexp.MustCompile(`<invoke\s+name=`)

// buildProxy 构造一个反向代理:透传一切,只在上游响应是 text/event-stream 时
// 进入 SSE 拦截(落盘样本 + 阶段二救援)。任何异常一律 fail-open。
func buildProxy(cfg *Config) *httputil.ReverseProxy {
	target := &url.URL{Scheme: cfg.UpstreamScheme, Host: cfg.UpstreamHost}

	rp := &httputil.ReverseProxy{
		// FlushInterval=-1:每次 Write 立即 flush,SSE 流不被缓冲。
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host // 重置 Host 为上游
			// 降级 Accept-Encoding:永不放行 br(避免任何解压麻烦);
			// Go transport 会在此值为空时自动加 gzip 并自动解压。
			req.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: modifyResponse(cfg),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] upstream error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":502,"message":"proxy upstream error"}`))
		},
	}
	return rp
}

func modifyResponse(cfg *Config) func(*http.Response) error {
	return func(resp *http.Response) error {
		// 顶层 fail-open:改写逻辑任何 panic 都不影响响应透传。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[proxy] modifyResponse panic (fail-open): %v", r)
			}
		}()

		if cfg.PassthroughOnly {
			return nil
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			return nil // 非流式:原样透传
		}

		meta := resp.Request.Method + " " + resp.Request.URL.Path +
			" status=" + resp.Status + " ct=" + ct +
			" ce=" + resp.Header.Get("Content-Encoding")

		orig := resp.Body
		pr, pw := io.Pipe()
		resp.Body = pr
		// 改写会变长度:去掉 Content-Length,让 Go 用 chunked 回给 CLI。
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1

		go streamSSE(cfg, meta, orig, pw)
		return nil
	}
}

// streamSSE 逐行读上游 SSE,落盘样本,(阶段二)按需救援,再写回下游管道。
// 阶段 0/1:纯透传 + 落盘。救援分支由 RescueEnabled 控制。
func streamSSE(cfg *Config, meta string, upstream io.ReadCloser, out *io.PipeWriter) {
	sw := newSampleWriter(cfg, meta)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[proxy] streamSSE panic (fail-open close): %v", r)
		}
		sw.close(cfg)
		_ = upstream.Close()
		_ = out.Close()
	}()

	reader := bufio.NewReader(upstream)

	if !cfg.RescueEnabled {
		// 阶段 0/1:逐行透传 + 落盘,零改写。同时嗅探退化信号并审计(不改写流)。
		buf := make([]byte, 32*1024)
		sniff := &degradeSniffer{}
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				sw.write(chunk)
				sniff.feed(chunk)
				if _, werr := out.Write(chunk); werr != nil {
					return // 下游断开(CLI 关闭),静默退出
				}
			}
			if err != nil {
				if sniff.hit {
					auditWrite(auditRecord{
						Result: "detected",
						Sample: filepath.Base(sw.path),
						Note:   "passthrough mode; run `claude-proxy analyze` on this sample",
					})
				}
				return
			}
		}
	}

	// 阶段 2:救援状态机(据样本填 rescue.go 后接入)。
	rescueStream(cfg, sw, reader, out)
}
