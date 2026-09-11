package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// analyzeSample 只读离线分析一个 .sse 样本文件,回答计划里的 U1–U6:
//   - 打印事件序列、每个 content_block 的 type/index
//   - 累积并打印每个 block 的 text / partial_json
//   - 打印 message_delta.stop_reason
//   - 高亮出现 invoke / count / court 的位置
func analyzeSample(path string) {
	fh, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer fh.Close()

	reader := bufio.NewReader(fh)

	// 跳过 # META 注释行(不是 SSE 事件)。
	// iterEvents 对非 event/data 行会忽略,但 META 行不含 event/data 前缀,
	// 会被并入某个事件的 raw;为清晰,先把首行 META 读掉打印。
	if peek, _ := reader.Peek(7); strings.HasPrefix(string(peek), "# META") {
		metaLine, _ := reader.ReadString('\n')
		fmt.Printf("META: %s\n", strings.TrimSpace(metaLine))
		fmt.Println(strings.Repeat("-", 70))
	}

	blockText := map[float64]*strings.Builder{} // index -> 累积文本
	blockType := map[float64]string{}           // index -> 类型
	var stopReason string
	evN := 0

	iterEvents(reader, func(ev sseEvent) bool {
		evN++
		switch ev.Event {
		case "content_block_start":
			idx := floatFrom(ev.Data, "index")
			cb, _ := ev.Data["content_block"].(map[string]interface{})
			typ, _ := cb["type"].(string)
			blockType[idx] = typ
			blockText[idx] = &strings.Builder{}
			name, _ := cb["name"].(string)
			id, _ := cb["id"].(string)
			extra := ""
			if typ == "tool_use" {
				extra = fmt.Sprintf(" name=%q id=%q", name, id)
			}
			fmt.Printf("[%02d] content_block_start  index=%v type=%s%s\n", evN, idx, typ, extra)
		case "content_block_delta":
			idx := floatFrom(ev.Data, "index")
			delta, _ := ev.Data["delta"].(map[string]interface{})
			dtyp, _ := delta["type"].(string)
			if b := blockText[idx]; b != nil {
				if t, ok := delta["text"].(string); ok {
					b.WriteString(t)
				}
				if pj, ok := delta["partial_json"].(string); ok {
					b.WriteString(pj)
				}
			}
			_ = dtyp
		case "content_block_stop":
			idx := floatFrom(ev.Data, "index")
			text := ""
			if b := blockText[idx]; b != nil {
				text = b.String()
			}
			typ := blockType[idx]
			flag := ""
			if hasDegradeSignal(text) {
				flag = "  <<< DEGRADE SIGNAL (count/court/invoke)"
			}
			fmt.Printf("[%02d] content_block_stop   index=%v type=%s len=%d%s\n", evN, idx, typ, len(text), flag)
			if text != "" {
				fmt.Printf("        accumulated: %s\n", preview(text, 400))
			}
		case "message_delta":
			delta, _ := ev.Data["delta"].(map[string]interface{})
			if sr, ok := delta["stop_reason"].(string); ok {
				stopReason = sr
			}
			fmt.Printf("[%02d] message_delta        stop_reason=%q\n", evN, stopReason)
		case "message_start", "message_stop", "ping":
			fmt.Printf("[%02d] %s\n", evN, ev.Event)
		default:
			if ev.Event != "" {
				fmt.Printf("[%02d] %s\n", evN, ev.Event)
			}
		}
		return true
	})

	fmt.Println(strings.Repeat("-", 70))
	fmt.Printf("总事件数=%d  最终 stop_reason=%q\n", evN, stopReason)
	fmt.Println("U1: 上面带 DEGRADE SIGNAL 的 block 的 type —— text 则为形态(a)可救; tool_use 则为形态(b)。")
}

func hasDegradeSignal(s string) bool {
	return strings.Contains(s, "<invoke name=") ||
		strings.Contains(s, "court") || strings.Contains(s, "count")
}

func floatFrom(m map[string]interface{}, k string) float64 {
	if m == nil {
		return -1
	}
	if f, ok := m[k].(float64); ok {
		return f
	}
	return -1
}

func preview(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > n {
		return s[:n] + fmt.Sprintf("...(+%d)", len(s)-n)
	}
	return s
}
