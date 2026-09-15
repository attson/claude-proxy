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

// orphanPrefixTail 匹配 text 尾部孤立成行/成尾的退化前缀 count/court:
// 前面必须是字符串起始或空白/换行,后面到结尾只能是空白。命中返回剥离前缀
// 及其前置空白后的文本。用于识别「text 尾部残留孤儿前缀 + 后接真 tool_use」形态。
var orphanPrefixTail = regexp.MustCompile(`(?:^|[\s])(?:count|court)\s*$`)

// stripOrphanPrefixTail 若 text 以孤立的 count/court 结尾,剥掉该前缀及其前导空白,
// 返回 (剥离后文本, true);否则返回 (原文, false)。
func stripOrphanPrefixTail(text string) (string, bool) {
	loc := orphanPrefixTail.FindStringIndex(text)
	if loc == nil {
		return text, false
	}
	// loc[0] 指向匹配起点(可能是前导空白或字符串起始);剥到该点并去掉尾部空白。
	return strings.TrimRight(text[:loc[0]], " \n\r\t"), true
}

// —— 刷屏式退化短句尾巴(spam tail)——
// 另一种退化形态:text block 尾部刷屏式连吐几十段极短祈使句
// (court read. / Grep. / Read. / Let me actually read.),后接一个结构完整的真
// tool_use。无 <invoke> XML,尾部也非单个孤立 count/court,前三条救援路径都够不着。
// 结构锚点:后接真 tool_use(见 settleOrphan 的 nextIsToolUse 门控),剥错不破坏工具调用。

const (
	spamTailMinSegs       = 6  // 尾部连续退化短句(整体)达到此段数即判刷屏
	spamTailMinPrefixSegs = 3  // 尾部连续退化段中 court/count 前缀段达到此数即判刷屏(强信号,降阈值)
	spamTailMaxSegLen     = 40 // 关键词「含」式短句单段(rune 计)上限
	spamImperativeMaxLen  = 64 // 祈使句「开头」式单段(rune 计)上限(略放宽,容纳 "I'll read the ... CSS.")
)

// spamPrefixRe 强退化信号:段以 count/court 前缀开头(不限长度,前缀本身即铁证)。
var spamPrefixRe = regexp.MustCompile(`(?i)^(?:count|court)`)

// spamImperativeStartRe 祈使句退化信号(以祈使词开头):Grep./Read./Run./Build./
// Reading./Running./Let me .../I'll .../I read .../I run .../Enough .../Now grep. 等。
// 配合 spamImperativeMaxLen 限长,容纳略长的刷屏变体如 "I'll read the ... CSS."。
var spamImperativeStartRe = regexp.MustCompile(`(?i)^(?:grep|read|run|build|running|reading|let me|i'll|i read|i run|now |enough)\b`)

// spamImperativeHasRe 更严格的「含关键词」退化信号,只用于极短段(<= spamTailMaxSegLen),
// 兜住 "Grep now." / "Read it." 这类祈使词不在句首的极短变体。
var spamImperativeHasRe = regexp.MustCompile(`(?i)\b(?:grep|read|run|build|running|reading)\b`)

// isSpamSeg 判定单段是否为退化短句。prefix 表示是否为 court/count 前缀段(强信号)。
func isSpamSeg(s string) (spam, prefix bool) {
	if spamPrefixRe.MatchString(s) {
		return true, true
	}
	n := len([]rune(s))
	if n <= spamImperativeMaxLen && spamImperativeStartRe.MatchString(s) {
		return true, false
	}
	if n <= spamTailMaxSegLen && spamImperativeHasRe.MatchString(s) {
		return true, false
	}
	return false, false
}

// detectSpamTail 判断 text 尾部是否为刷屏式退化短句序列。
// 命中返回切点前的正常叙述(TrimRight 空白后)与 true;否则返回原文与 false。
// 判据:按 \n\n 切段,从末段起向前数连续「退化短句」;整体段数 >= spamTailMinSegs,
// 或其中 court/count 前缀段数 >= spamTailMinPrefixSegs(强信号降阈值),二者任一即命中。
func detectSpamTail(text string) (narrative string, matched bool) {
	segs := strings.Split(text, "\n\n")
	// 从末尾向前数连续命中的退化短句段,并单独计 court/count 前缀段数
	run, prefixRun := 0, 0
	for i := len(segs) - 1; i >= 0; i-- {
		s := strings.TrimSpace(segs[i])
		if s == "" {
			continue // 空段跳过,不打断连续性
		}
		spam, prefix := isSpamSeg(s)
		if !spam {
			break
		}
		run++
		if prefix {
			prefixRun++
		}
	}
	if run < spamTailMinSegs && prefixRun < spamTailMinPrefixSegs {
		return text, false
	}
	// 切点:保留前 len(segs)-run 段作为正常叙述
	pre := strings.Join(segs[:len(segs)-run], "\n\n")
	return strings.TrimRight(pre, " \n\r\t"), true
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

	// 挂起的「尾部孤儿前缀」text block:text 已结束且尾部含孤立 count/court,
	// 但还需看下一个 block 是否为 tool_use 才能决定剥离。此时暂不发出。
	var pendingOrphan bool
	var pendingOrphanText string // 已剥离尾部前缀后的叙述文字
	var pendingOrphanRaw string  // 原始文字(含前缀),用于「下一个不是 tool_use」时回退
	var pendingOrphanIndex int   // 该 text block 的原始 index

	// 当前正在缓冲的 tool_use block(仅已知会双重编码 input 的工具)
	var tuBuffering bool
	var tuName string
	var tuStartEv []byte // 已 reindex 的 content_block_start 原始字节
	var tuJSON strings.Builder

	// handleToolStop 处理已知工具的 tool_use block 结束:修 input 后重发。
	handleToolStop := func(stopEv sseEvent) (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[proxy] tool input fix panic (fail-open): %v", r)
				// fail-open:补发缓冲的原始 start + delta + stop
				idx := floatFrom(stopEv.Data, "index") + float64(indexShift)
				ok = write(tuStartEv) &&
					writeInputDelta(write, idx, tuJSON.String()) &&
					write(reindex(stopEv, indexShift))
				tuBuffering = false
			}
		}()
		raw := []byte(tuJSON.String())
		fixed, changed := fixToolInput(tuName, raw)
		startBytes := tuStartEv
		tuBuffering = false
		// start/delta/stop 三者 index 必须一致:统一用加过偏移的 index。
		idx := floatFrom(stopEv.Data, "index") + float64(indexShift)
		payload := string(raw)
		if changed {
			payload = string(fixed)
			rescued = true
			auditWrite(auditRecord{Result: "input_fixed", Tool: tuName, Sample: baseName(sw)})
		}
		return write(startBytes) &&
			writeInputDelta(write, idx, payload) &&
			write(reindex(stopEv, indexShift))
	}

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
			// 无 <invoke> XML,但可能是「尾部孤儿前缀 + 后接真 tool_use」形态:
			// 挂起等待下一个 block 判定,不立即 flush。
			if stripped, matched := stripOrphanPrefixTail(full); matched {
				pendingOrphan = true
				pendingOrphanText = stripped
				pendingOrphanRaw = full
				pendingOrphanIndex = int(bufIndex)
				buffering = false
				bufEvents = nil
				bufText.Reset()
				return true
			}
			// 「刷屏式退化短句尾巴 + 后接真 tool_use」形态:同样挂起等下一个 block 判定。
			if narrative, matched := detectSpamTail(full); matched {
				pendingOrphan = true
				pendingOrphanText = narrative
				pendingOrphanRaw = full
				pendingOrphanIndex = int(bufIndex)
				buffering = false
				bufEvents = nil
				bufText.Reset()
				return true
			}
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

	// settleOrphan 结算挂起的孤儿前缀 text block。nextIsToolUse=true 表示紧跟
	// 一个 tool_use(剥离生效,发剥离后叙述);否则回退发原文(剥离作废)。
	settleOrphan := func(nextIsToolUse bool) bool {
		pendingOrphan = false
		text := pendingOrphanRaw
		if nextIsToolUse {
			text = pendingOrphanText
			rescued = true
			auditWrite(auditRecord{Result: "orphan_stripped", BlockIndex: pendingOrphanIndex, Sample: baseName(sw)})
		}
		if strings.TrimSpace(text) == "" {
			return true // 剥离后为空:整个 text block 丢弃,无需发出
		}
		return emitTextBlock(write, pendingOrphanIndex, text)
	}

	ok := true
	iterEvents(reader, func(ev sseEvent) bool {
		if !ok {
			return false
		}
		// 有挂起的孤儿前缀 text block:任何后续事件到来先结算它。
		// 仅当紧跟的是 tool_use 的 content_block_start 才剥离生效。
		if pendingOrphan {
			nextIsToolUse := false
			if ev.Event == "content_block_start" {
				if typ, _ := blockType(ev); typ == "tool_use" {
					nextIsToolUse = true
				}
			}
			if !settleOrphan(nextIsToolUse) {
				ok = false
				return false
			}
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
			// tool_use 且属于已知会双重编码的工具:缓冲以便修 input
			if typ == "tool_use" {
				if name, _ := toolName(ev); knownDoubleEncoded[name] != nil {
					tuBuffering = true
					tuName = name
					tuStartEv = reindex(ev, indexShift)
					tuJSON.Reset()
					return true
				}
			}
			// 其它 block:若之前拆块导致 index 偏移,改写 index
			ok = write(reindex(ev, indexShift))
			return ok

		case "content_block_delta":
			if tuBuffering {
				if pj := deltaPartialJSON(ev); pj != "" {
					tuJSON.WriteString(pj)
				}
				return true // delta 先不发,等 stop 时决定
			}
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
			if tuBuffering {
				ok = handleToolStop(ev)
				return ok
			}
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
	// 挂起的孤儿前缀未结算(异常中断:后面没有任何事件):回退发原文,不剥离。
	if pendingOrphan {
		_ = settleOrphan(false)
	}
	// tool_use 缓冲未闭合(异常中断):原样补发 start + delta(不修)
	if tuBuffering {
		_ = write(tuStartEv)
		if tuJSON.Len() > 0 {
			delta := map[string]interface{}{
				"type": "content_block_delta", "index": 0.0,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tuJSON.String()},
			}
			_ = write(formatEvent("content_block_delta", delta))
		}
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

func deltaPartialJSON(ev sseEvent) string {
	d, _ := ev.Data["delta"].(map[string]interface{})
	if t, ok := d["partial_json"].(string); ok {
		return t
	}
	return ""
}

func toolName(ev sseEvent) (string, bool) {
	cb, _ := ev.Data["content_block"].(map[string]interface{})
	n, ok := cb["name"].(string)
	return n, ok
}

// writeInputDelta 用给定 partial_json 合成并写出一个 input_json_delta 事件。
func writeInputDelta(write func([]byte) bool, index float64, partialJSON string) bool {
	delta := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": partialJSON},
	}
	return write(formatEvent("content_block_delta", delta))
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
