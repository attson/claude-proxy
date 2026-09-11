package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 救援/检测审计日志。独立于 proxy.log,每行一条 JSON,**只含结构化元信息,
// 绝不含工具参数值**(避免把 command 等明文落到第二个文件)。
//
// 结果分类:
//
//	detected  阶段0/1:透传模式下检测到退化信号(帮助阶段一定位样本)
//	success   阶段2:成功解析伪 invoke 并合成 tool_use
//	parse_failed 阶段2:命中退化但解析失败,已 fail-open 透传
//	failopen  阶段2:救援过程异常,已 fail-open 透传
type auditRecord struct {
	Time       string `json:"time"`
	Result     string `json:"result"`
	Tool       string `json:"tool,omitempty"`        // 解析出的工具 name(不含参数值)
	ParamCount int    `json:"param_count,omitempty"` // 参数个数
	BlockIndex int    `json:"block_index,omitempty"`
	ToolID     string `json:"tool_id,omitempty"` // 合成的 toolu_ id
	Sample     string `json:"sample,omitempty"`  // 对应样本文件名,便于回查
	Note       string `json:"note,omitempty"`
}

var auditMu sync.Mutex
var auditPath string

func initAudit(cfg *Config) {
	auditPath = filepath.Join(filepath.Dir(cfg.LogFile), "audit.log")
}

func auditWrite(rec auditRecord) {
	if auditPath == "" {
		return
	}
	rec.Time = time.Now().Format(time.RFC3339)
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	defer func() { _ = recover() }()
	fh, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(line, '\n'))
}
