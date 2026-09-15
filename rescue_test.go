package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// runRescue 用真实/构造样本驱动 rescueStream,返回改写后的完整输出。
func runRescue(t *testing.T, input string) string {
	cfg := &Config{SampleEnabled: false, RescueEnabled: true}
	sw := &sampleWriter{} // 不落盘
	reader := bufio.NewReader(strings.NewReader(input))
	pr, pw := io.Pipe()
	go func() {
		rescueStream(cfg, sw, reader, pw)
		pw.Close() // 救援结束后关闭写端,否则 ReadAll 永远阻塞
	}()
	out, _ := io.ReadAll(pr)
	return string(out)
}

// textToolStopEvents 把「一个 text block(单个 delta)+ 收尾 message_delta」包装成 SSE 事件流。
// stopReason 为上游最终 stop_reason。用于构造退化样本,数据源保真但不依赖易失的采样文件。
func textBlockSSE(text, stopReason string) string {
	return strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonStr(text) + `}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":` + jsonStr(stopReason) + `}}`,
		``,
		``,
	}, "\n")
}

// 构造退化样本(形态取自 trace 历史真实响应):text block 内含叙述 + 孤儿 court + 伪 <invoke> XML,
// 无独立 tool_use。期望:救援重组出合成 tool_use(Bash),叙述保留,command 还原,stop_reason 改为 tool_use。
func TestRescueXMLInvoke(t *testing.T) {
	// 退化文本结构照搬真实现场:叙述段 + 孤儿 court 换行 + <invoke name="Bash"> XML。
	text := "**Task 8**:同步 lockfile。\n\ncourt\n" +
		`<invoke name="Bash">` + "\n" +
		`<parameter name="command">SDD_SKILL=1 pnpm install 2>&1 | tail -20</parameter>` + "\n" +
		`<parameter name="description">pnpm install 同步依赖</parameter>` + "\n" +
		`</invoke>`
	out := runRescue(t, textBlockSSE(text, "end_turn"))

	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("未合成 tool_use Bash block\n输出片段:\n%s", tail(out, 800))
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason 未改成 tool_use")
	}
	if !strings.Contains(out, "SDD_SKILL=") {
		t.Errorf("command 参数未正确还原")
	}
	if !strings.Contains(out, "Task 8") {
		t.Errorf("叙述文字未保留")
	}
	assertPartialJSONValid(t, out)
}

// 构造样本:纯正常 tool_use,不应被改写、不应误伤。
func TestRescueNormalToolUseUntouched(t *testing.T) {
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)
	// 应原样含 toolu_x(未被替换成合成 id)
	if !strings.Contains(out, "toolu_x") {
		t.Errorf("正常 tool_use 被误改\n%s", out)
	}
}

// 构造样本:纯正常 text(讨论 count/court/invoke 语法),不应误伤。
func TestRescueNormalTextUntouched(t *testing.T) {
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"这个 bug 会输出 count 或 court,讨论 invoke 语法"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Errorf("正常 text 被误判为退化并合成了 tool_use\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("正常 text 的 stop_reason 被误改")
	}
}

// 构造样本:text block 结尾一个孤儿 "court",紧跟一个结构完整的真 tool_use。
// 这是高频退化形态(text 里无 <invoke> XML,真 tool_use 独立成 block)。
// 期望:剥掉尾部孤儿前缀,保留正常叙述;真 tool_use 原样透传;不合成、不改 stop_reason。
func TestRescueOrphanPrefixBeforeToolUse(t *testing.T) {
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Spec 写好，直接派 task 执行。\n\ncourt"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_real","name":"Bash","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)

	// 正常叙述保留
	if !strings.Contains(out, "Spec 写好") {
		t.Errorf("正常叙述未保留\n%s", out)
	}
	// 尾部孤儿 court 被剥掉:输出的 text_delta 里不应再有独立的 court
	if strings.Contains(out, `court`) {
		t.Errorf("尾部孤儿 court 未被剥离\n%s", out)
	}
	// 真 tool_use 原样透传(id 不被替换)
	if !strings.Contains(out, "toolu_real") {
		t.Errorf("真 tool_use 被误改或丢失\n%s", out)
	}
	// 上游本就是 tool_use,stop_reason 不变
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason 被误改\n%s", out)
	}
}

// 构造退化样本:text block 内容纯为孤儿 "court"(叙述为空),紧跟完整 AskUserQuestion tool_use。
// 期望:整个孤儿 text block 丢弃(剥完为空),AskUserQuestion 原样透传、id 不变。
func TestRescueOrphanPrefixBeforeAsk(t *testing.T) {
	const askID = "toolu_01PAE9uHNHXc1cy3CFerMiz8"
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"court"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"` + askID + `","name":"AskUserQuestion","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"questions\":[]}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)

	// 孤儿 court 不应作为 text_delta 泄漏出去
	for _, txt := range textDeltas(out) {
		if strings.TrimSpace(txt) == "court" {
			t.Errorf("孤儿 court text_delta 泄漏: %q", txt)
		}
	}
	if !strings.Contains(out, "AskUserQuestion") {
		t.Errorf("AskUserQuestion tool_use 丢失\n%s", tail(out, 400))
	}
	if !strings.Contains(out, askID) {
		t.Errorf("AskUserQuestion 原 id 被改\n%s", tail(out, 400))
	}
}

// 防误伤:text 尾部有孤立成行的 "court",但后面不是 tool_use(而是 end_turn 收尾)。
// 「孤立成行 + 后接真 tool_use」两条须同时满足才剥;这里缺后者,不剥,原样透传。
func TestRescueTailWordNotStrippedWithoutToolUse(t *testing.T) {
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"网球场地英文单词是\n\ncourt"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)
	if !strings.Contains(out, "网球场地英文单词是") {
		t.Errorf("叙述被误删\n%s", out)
	}
	// 后面无 tool_use → 不剥,court 原样保留
	if !strings.Contains(out, "court") {
		t.Errorf("后面无 tool_use 时不应剥离尾部 court\n%s", out)
	}
}

// 单元测试 stripOrphanPrefixTail:只有尾部孤立的 count/court 才剥,
// discount/account 等以 count 结尾的正常单词不误伤。
func TestStripOrphanPrefixTail(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		matched bool
	}{
		{"court", "", true},                   // 纯前缀
		{"叙述。\n\ncourt", "叙述。", true},         // 叙述 + 换行 + 孤儿
		{"叙述 count", "叙述", true},              // 空格分隔孤儿
		{"这是 discount", "这是 discount", false}, // discount 结尾不是孤立 count
		{"my account", "my account", false},   // account 结尾不是孤立 count
		{"正常结尾。", "正常结尾。", false},             // 无前缀
		{"court 后面还有字", "court 后面还有字", false}, // 前缀不在尾部
	}
	for _, c := range cases {
		got, m := stripOrphanPrefixTail(c.in)
		if m != c.matched || (m && got != c.want) {
			t.Errorf("in=%q => got=%q matched=%v, want=%q matched=%v", c.in, got, m, c.want, c.matched)
		}
	}
}

func assertPartialJSONValid(t *testing.T, out string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var d map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &d) != nil {
			continue
		}
		delta, _ := d["delta"].(map[string]interface{})
		if pj, ok := delta["partial_json"].(string); ok {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(pj), &m); err != nil {
				t.Errorf("partial_json 不是合法 JSON: %q err=%v", pj, err)
			}
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// —— 刷屏式退化短句尾巴(spam tail)——

// spamSeg 构造 n 段以 \n\n 分隔的刷屏退化短句(court/Grep./Read. 循环),用于测试。
func spamSeg(n int) string {
	segs := []string{"court read.", "Read.", "court grep.", "Grep.", "Let me read.", "I'll read."}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(segs[i%len(segs)])
	}
	return b.String()
}

// TestDetectSpamTail 判据单元测试:正例识别刷屏尾巴并切出正常叙述,反例不误伤。
func TestDetectSpamTail(t *testing.T) {
	narrative := "先看 MyMRPanel.vue 现在的行渲染模板（单行卡那段）。"
	cases := []struct {
		name    string
		in      string
		matched bool
		want    string // matched 时期望切出的叙述(TrimRight 后)
	}{
		{"刷屏尾巴+正常叙述", narrative + "\n\n" + spamSeg(20), true, narrative},
		{"纯刷屏无叙述", spamSeg(12), true, ""},
		{"恰好阈值段数", narrative + "\n\n" + spamSeg(spamTailMinSegs), true, narrative},
		// 反例:防误伤
		{"少量提及court非刷屏", "网球场地英文是 court，这是正常讨论。", false, ""},
		{"不足阈值的短句", narrative + "\n\ncourt read.\n\nRead.", false, ""},
		{"分点长句回答", "第一点是这样一个较长的完整句子用于占位说明。\n\n第二点也是一个足够长的完整句子避免被判成短句。\n\n第三点同样是一个明显超过四十字上限的正常叙述句子。", false, ""},
		{"正常text讨论invoke语法", "这个 bug 会输出 count 或 court,讨论 invoke 语法", false, ""},
	}
	for _, c := range cases {
		got, m := detectSpamTail(c.in)
		if m != c.matched {
			t.Errorf("%s: matched=%v want=%v (in=%q)", c.name, m, c.matched, c.in)
			continue
		}
		if m && strings.TrimRight(got, " \n\r\t") != c.want {
			t.Errorf("%s: narrative=%q want=%q", c.name, got, c.want)
		}
	}
}

// TestRescueSpamTailBeforeToolUse 端到端:刷屏尾巴 text block 紧跟真 tool_use → 剥尾、保叙述、透传 tool_use。
func TestRescueSpamTailBeforeToolUse(t *testing.T) {
	text := "先看行渲染模板。" + "\n\n" + spamSeg(20)
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonStr(text) + `}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_real","name":"Read","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"offset\":273}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)

	if !strings.Contains(out, "先看行渲染模板") {
		t.Errorf("正常叙述未保留\n%s", out)
	}
	if strings.Contains(out, "court") || strings.Contains(out, "Grep.") {
		t.Errorf("刷屏尾巴未被剥离\n%s", out)
	}
	if !strings.Contains(out, "toolu_real") {
		t.Errorf("真 tool_use 被误改或丢失\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason 被误改\n%s", out)
	}
}

// TestRescueSpamTailNotStrippedWithoutToolUse 防误伤:刷屏尾巴后接 end_turn(无 tool_use)→ 不剥,原样透传。
func TestRescueSpamTailNotStrippedWithoutToolUse(t *testing.T) {
	text := "先看行渲染模板。" + "\n\n" + spamSeg(20)
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonStr(text) + `}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)
	if !strings.Contains(out, "先看行渲染模板") {
		t.Errorf("叙述被误删\n%s", out)
	}
	// 后面无 tool_use → 不剥,刷屏原样保留
	if !strings.Contains(out, "court") {
		t.Errorf("后接非 tool_use 时不应剥离刷屏尾巴\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason 被误改\n%s", out)
	}
}

// TestRescueSpamTailRealSample 多个真样本回归:满屏 court 刷屏被剥,真 tool_use 完整透传。
// 覆盖三个现场:291(Read 循环)、282(court read anchor)、338(court看 CSS,叙述含 court 需保留一部分)。
func TestRescueSpamTailRealSample(t *testing.T) {
	// 退化正例 00552:满屏 court read 刷屏 + 后接真 tool_use(Read),期望剥屏、保叙述、透传工具。
	out := runRescue(t, readTestdataSSE(t, "20260915-112709-00552.sse"))
	for _, txt := range textDeltas(out) {
		if strings.Contains(txt, "\n\ncourt") {
			t.Errorf("[00552] 刷屏 court text_delta 泄漏: %q", tail(txt, 120))
		}
	}
	if !strings.Contains(out, `"name":"Read"`) {
		t.Errorf("[00552] 真 tool_use Read 丢失\n%s", tail(out, 600))
	}
	if !strings.Contains(out, "atwebpilot 在这个环境彻底不可用") {
		t.Errorf("[00552] 正常叙述未保留\n%s", tail(out, 600))
	}

	// 防误伤反例:正常长文本高频提及 "court"/"read"/"grep" 等词但非刷屏(长句、非短祈使),
	// 且后接真 tool_use。判据不得剥离,叙述须完整保留。
	narrative := "好方向:court/count 前缀是强退化信号,我打算把判据分成两档来识别刷屏。" +
		"\n\n先读一遍现有的 rescue 逻辑,再决定 grep 哪些样本来验证阈值是否合理。"
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonStr(narrative) + `}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_real2","name":"Edit","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out2 := runRescue(t, input)
	if !strings.Contains(out2, "先读一遍现有的 rescue 逻辑") {
		t.Errorf("正常长叙述(含 court/read/grep 词)被误剥\n%s", tail(out2, 600))
	}
	if !strings.Contains(out2, "toolu_real2") {
		t.Errorf("真 tool_use Edit 被误改或丢失\n%s", tail(out2, 600))
	}
}

// readTestdataSSE 读入库的 testdata SSE 样本,去掉 # META 首行,返回纯 SSE 体。
func readTestdataSSE(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读 testdata/%s 失败: %v", name, err)
	}
	if strings.HasPrefix(string(data), "# META") {
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			return string(data[i+1:])
		}
	}
	return string(data)
}

// textDeltas 从救援输出里提取所有 text_delta 的文本片段。
func textDeltas(out string) []string {
	var res []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var d map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &d) != nil {
			continue
		}
		if delta, _ := d["delta"].(map[string]interface{}); delta != nil {
			if txt, ok := delta["text"].(string); ok {
				res = append(res, txt)
			}
		}
	}
	return res
}

// jsonStr 把字符串编码为 JSON 字符串字面量(带引号),用于拼构造样本。
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 构造坏样本:AskUserQuestion 的 questions 被双重编码(值是被转义的 JSON 字符串而非数组),
// 救援后应还原成合法数组(input_json_delta 里 questions 是 array)。
func TestRescueInputFixDoubleEncoded(t *testing.T) {
	// 双重编码:questions 的值本应是数组,却被编码成一个 JSON 字符串。
	// 先造出正确的 input JSON({"questions":"<数组的JSON字符串>"}),再把它作为 partial_json 的值
	// 用 jsonStr 编码进 data 行,避免手写多层转义出错。
	innerArr := `[{"question":"选哪个?","header":"选择","multiSelect":false,"options":[{"label":"A","description":"甲"},{"label":"B","description":"乙"}]}]`
	inputJSON, _ := json.Marshal(map[string]interface{}{"questions": innerArr}) // questions 值是字符串 = 双重编码
	input := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ask","name":"AskUserQuestion","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + jsonStr(string(inputJSON)) + `}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		``,
	}, "\n")
	out := runRescue(t, input)

	// 从输出里找 AskUserQuestion 的 input_json_delta,确认 questions 已是数组
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var d map[string]interface{}
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &d) != nil {
			continue
		}
		delta, _ := d["delta"].(map[string]interface{})
		pj, ok := delta["partial_json"].(string)
		if !ok || !strings.Contains(pj, "questions") {
			continue
		}
		var input map[string]interface{}
		if json.Unmarshal([]byte(pj), &input) != nil {
			continue
		}
		if _, isArr := input["questions"].([]interface{}); isArr {
			found = true
		} else {
			t.Errorf("questions 仍不是数组: %T", input["questions"])
		}
	}
	if !found {
		t.Fatalf("未在救援输出里找到修复后的 AskUserQuestion input")
	}
}
