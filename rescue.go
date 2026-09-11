package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"strings"
)

// 退化检测(在**还原后的完整文本**上跑,不是流字节):
// 真实形态为 "...court\n\ncount\n<invoke name=\"Bash\">...</invoke>"(无 <function_calls> 包裹)。
// 结构信号 <invoke name=" 最稳,配合前缀 count/court 降低误伤。
var invokeStart = regexp.MustCompile(`<invoke\s+name="([^"]+)"\s*>`)
var paramRe = regexp.MustCompile(`(?s)<parameter\s+name="([^"]+)"\s*>(.*?)</parameter>`)

// degradeText 判断一个已还原的完整文本是否含退化的伪 invoke。
func degradeText(text string) bool {
	return invokeStart.MatchString(text)
}

// computeSplit 返回 (叙述文字, 是否找到伪invoke)。叙述文字是 <invoke 之前、
// 去掉尾部孤立 count/court 与空白后的正常内容。
func computeSplit(text string) (narrative string, ok bool) {
	loc := invokeStart.FindStringIndex(text)
	if loc == nil {
		return "", false
	}
	pre := text[:loc[0]]
	// 去掉尾部的孤立 count/court 行与空白
	pre = strings.TrimRight(pre, " \n\r\t")
	for {
		trimmed := false
		for _, m := range []string{"count", "court"} {
			if strings.HasSuffix(pre, m) {
				pre = strings.TrimRight(pre[:len(pre)-len(m)], " \n\r\t")
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	return pre, true
}

// parsePseudoInvoke 从退化文本解析出工具 name 与 input(全部当字符串,值保留原文)。
func parsePseudoInvoke(text string) (name string, input map[string]string, ok bool) {
	nm := invokeStart.FindStringSubmatch(text)
	if nm == nil {
		return "", nil, false
	}
	name = nm[1]
	input = map[string]string{}
	for _, m := range paramRe.FindAllStringSubmatch(text, -1) {
		input[m[1]] = m[2]
	}
	return name, input, true
}

// genToolID 生成仿 Anthropic 的 toolu_ id。
func genToolID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "toolu_" + string(b)
}

// rescueStream 是阶段 2 救援状态机:逐事件处理,text block 缓冲到 stop 判定,
// 退化则拆块(叙述 text + 合成 tool_use)并给后续 block index +1;否则原样补发。
// 全程 fail-open:任何异常回退到补发缓冲的原始事件。
func rescueStream(cfg *Config, sw *sampleWriter, reader *bufio.Reader, out *io.PipeWriter) {
	indexShift := 0 // 因拆块新增 block 后,后续 block 的 index 偏移量
	rescued := false

	write := func(b []byte) bool {
		sw.write(b)
		_, err := out.Write(b)
		return err == nil
	}

	// 当前正在缓冲的 text block(仅当首个 delta 嗅到 <invoke 才缓冲)
	var buffering bool
	var bufEvents [][]byte // 缓冲的原始事件字节(用于 fail-open 补发)
	var bufText strings.Builder
	var bufIndex float64

	flushBufferAsIs := func() bool {
		for _, e := range bufEvents {
			if !write(e) {
				return false
			}
		}
		bufEvents = nil
		bufText.Reset()
		buffering = false
		return true
	}

	handleStop := func() bool {
		// 到达被缓冲 text block 的 stop:判定退化与否
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[proxy] rescue panic (fail-open): %v", r)
				auditWrite(auditRecord{Result: "failopen", Sample: baseName(sw), Note: "panic"})
				_ = flushBufferAsIs()
			}
		}()
		full := bufText.String()
		if !degradeText(full) {
			return flushBufferAsIs() // 非退化,原样补发
		}
		narrative, ok := computeSplit(full)
		name, input, ok2 := parsePseudoInvoke(full)
		if !ok || !ok2 || name == "" {
			auditWrite(auditRecord{Result: "parse_failed", Sample: baseName(sw)})
			return flushBufferAsIs()
		}
		idx := int(bufIndex)
		// 1) 叙述文字仍作 text block 发回(若有内容)
		if strings.TrimSpace(narrative) != "" {
			if !emitTextBlock(write, idx, narrative) {
				return false
			}
			idx++
			indexShift++ // 新增了一个 block,后续 index 全部 +1
		}
		// 2) 合成 tool_use block
		toolID := genToolID()
		if !emitToolUse(write, idx, toolID, name, input) {
			return false
		}
		rescued = true
		buffering = false
		bufEvents = nil
		bufText.Reset()
		auditWrite(auditRecord{
			Result: "success", Tool: name, ParamCount: len(input),
			BlockIndex: idx, ToolID: toolID, Sample: baseName(sw),
		})
		return true
	}

	ok := true
	iterEvents(reader, func(ev sseEvent) bool {
		if !ok {
			return false
		}
		switch ev.Event {
		case "content_block_start":
			typ, _ := blockType(ev)
			if typ == "text" {
				// 开始一个 text block:先缓冲,待首个 delta 嗅探决定是否继续缓冲
				buffering = true
				bufEvents = [][]byte{ev.Raw}
				bufText.Reset()
				bufIndex = floatFrom(ev.Data, "index")
				return true
			}
			// 非 text block:若之前在拆块导致 index 偏移,需改写 index
			ok = write(reindex(ev, indexShift))
			return ok

		case "content_block_delta":
			if buffering {
				bufEvents = append(bufEvents, ev.Raw)
				if t := deltaText(ev); t != "" {
					bufText.WriteString(t)
					// 首次嗅到 <invoke 才值得继续缓冲;否则立即放弃缓冲、原样补发
					if !invokeStart.MatchString(bufText.String()) && bufText.Len() > 8192 {
						// 已累积不少却无 invoke 迹象 → 大概率正常,放弃缓冲省内存
						ok = flushBufferAsIs()
					}
				}
				return ok
			}
			ok = write(reindex(ev, indexShift))
			return ok

		case "content_block_stop":
			if buffering {
				bufEvents = append(bufEvents, ev.Raw)
				ok = handleStop()
				return ok
			}
			ok = write(reindex(ev, indexShift))
			return ok

		case "message_delta":
			// 若本回合发生了救援,把 stop_reason 改成 tool_use(U4 稳妥处理)
			if rescued {
				if d, has := ev.Data["delta"].(map[string]interface{}); has {
					d["stop_reason"] = "tool_use"
				}
				ok = write(formatEvent(ev.Event, ev.Data))
				return ok
			}
			ok = write(ev.Raw)
			return ok

		default:
			ok = write(ev.Raw)
			return ok
		}
	})

	// 流结束时若仍有未 flush 的缓冲(异常中断),补发
	if buffering && len(bufEvents) > 0 {
		_ = flushBufferAsIs()
	}
}

// —— 辅助 ——

func blockType(ev sseEvent) (string, bool) {
	cb, _ := ev.Data["content_block"].(map[string]interface{})
	t, ok := cb["type"].(string)
	return t, ok
}

func deltaText(ev sseEvent) string {
	d, _ := ev.Data["delta"].(map[string]interface{})
	if t, ok := d["text"].(string); ok {
		return t
	}
	return ""
}

// reindex 若 shift>0,改写事件里的 index 字段后重新序列化;否则原样返回 Raw。
func reindex(ev sseEvent, shift int) []byte {
	if shift == 0 || ev.Data == nil {
		return ev.Raw
	}
	if idx, ok := ev.Data["index"].(float64); ok {
		ev.Data["index"] = idx + float64(shift)
		return formatEvent(ev.Event, ev.Data)
	}
	return ev.Raw
}

func emitTextBlock(write func([]byte) bool, index int, text string) bool {
	start := map[string]interface{}{
		"type": "content_block_start", "index": index,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}
	delta := map[string]interface{}{
		"type": "content_block_delta", "index": index,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	}
	stop := map[string]interface{}{"type": "content_block_stop", "index": index}
	return write(formatEvent("content_block_start", start)) &&
		write(formatEvent("content_block_delta", delta)) &&
		write(formatEvent("content_block_stop", stop))
}

func emitToolUse(write func([]byte) bool, index int, id, name string, input map[string]string) bool {
	start := map[string]interface{}{
		"type": "content_block_start", "index": index,
		"content_block": map[string]interface{}{
			"type": "tool_use", "id": id, "name": name, "input": map[string]interface{}{},
		},
	}
	js, _ := json.Marshal(input)
	delta := map[string]interface{}{
		"type": "content_block_delta", "index": index,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(js)},
	}
	stop := map[string]interface{}{"type": "content_block_stop", "index": index}
	return write(formatEvent("content_block_start", start)) &&
		write(formatEvent("content_block_delta", delta)) &&
		write(formatEvent("content_block_stop", stop))
}

func baseName(sw *sampleWriter) string {
	if sw == nil || sw.path == "" {
		return ""
	}
	return filepath.Base(sw.path)
}
