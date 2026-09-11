package main

import (
	"bufio"
	"encoding/json"
	"strings"
)

// sseEvent 是一个解析后的 SSE 事件。
type sseEvent struct {
	Event string                 // event: 行的值(可能为空)
	Data  map[string]interface{} // data: 行 JSON 解析结果(失败为 nil)
	Raw   []byte                 // 该事件原始字节(含结尾空行),用于 fail-open 原样补发
}

// iterEvents 从 bufio.Reader 逐个解析 SSE 事件,通过回调交付。
// 回调返回 false 可提前停止。永不 panic 导致中断。
func iterEvents(reader *bufio.Reader, fn func(sseEvent) bool) {
	var raw []byte
	var eventName string
	var dataLines [][]byte

	flush := func() bool {
		if len(raw) == 0 {
			return true
		}
		ev := sseEvent{Event: eventName, Raw: append([]byte(nil), raw...)}
		if len(dataLines) > 0 {
			joined := strings.Join(bytesToStrs(dataLines), "\n")
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(joined), &m); err == nil {
				ev.Data = m
			}
		}
		raw = raw[:0]
		eventName = ""
		dataLines = dataLines[:0]
		return fn(ev)
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw = append(raw, line...)
			stripped := strings.TrimRight(string(line), "\r\n")
			switch {
			case stripped == "":
				if !flush() {
					return
				}
			case strings.HasPrefix(stripped, "event:"):
				eventName = strings.TrimSpace(stripped[len("event:"):])
			case strings.HasPrefix(stripped, "data:"):
				dataLines = append(dataLines, []byte(strings.TrimSpace(stripped[len("data:"):])))
			}
		}
		if err != nil {
			flush()
			return
		}
	}
}

func bytesToStrs(bs [][]byte) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = string(b)
	}
	return out
}

// formatEvent 把一个事件序列化回 SSE 字节。
func formatEvent(event string, data map[string]interface{}) []byte {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	j, _ := json.Marshal(data)
	b.WriteString("data: ")
	b.Write(j)
	b.WriteString("\n\n")
	return []byte(b.String())
}
