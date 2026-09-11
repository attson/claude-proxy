package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// SampleWriter 把一次响应的原始 SSE(脱敏后)追加写到一个文件。
type sampleWriter struct {
	fh     *os.File
	path   string
	redact bool
}

var sampleSeq uint64
var rotateMu sync.Mutex

func newSampleWriter(cfg *Config, meta string) *sampleWriter {
	if !cfg.SampleEnabled {
		return &sampleWriter{}
	}
	seq := atomic.AddUint64(&sampleSeq, 1)
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(cfg.SampleDir, fmt.Sprintf("%s-%05d.sse", ts, seq))
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return &sampleWriter{}
	}
	sw := &sampleWriter{fh: fh, path: path, redact: cfg.RedactEnabled}
	sw.writeRaw(append([]byte("# META: "), append(sw.maybeRedact([]byte(meta)), '\n')...))
	return sw
}

// maybeRedact 按开关决定是否脱敏。默认(redact=true)脱敏。
func (s *sampleWriter) maybeRedact(b []byte) []byte {
	if s.redact {
		return redact(b)
	}
	return b
}

func (s *sampleWriter) write(raw []byte) {
	if s.fh == nil {
		return
	}
	s.writeRaw(s.maybeRedact(raw))
}

func (s *sampleWriter) writeRaw(b []byte) {
	if s.fh == nil {
		return
	}
	defer func() { _ = recover() }()
	_, _ = s.fh.Write(b)
}

func (s *sampleWriter) close(cfg *Config) {
	if s.fh != nil {
		_ = s.fh.Close()
		s.fh = nil
	}
	rotate(cfg)
}

// rotate 只保留最近 cfg.SampleKeep 个样本文件。
func rotate(cfg *Config) {
	rotateMu.Lock()
	defer rotateMu.Unlock()
	defer func() { _ = recover() }()

	entries, err := os.ReadDir(cfg.SampleDir)
	if err != nil {
		return
	}
	type fi struct {
		path string
		mod  time.Time
	}
	var files []fi
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sse" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{filepath.Join(cfg.SampleDir, e.Name()), info.ModTime()})
	}
	if len(files) <= cfg.SampleKeep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files[:len(files)-cfg.SampleKeep] {
		_ = os.Remove(f.path)
	}
}
