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

// 用真实退化样本 00136 驱动,验证重组出正确的 tool_use。
func TestRescueRealSample(t *testing.T) {
	path := os.ExpandEnv("$HOME/.claude-proxy/samples/20260911-111751-00136.sse")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("样本不存在,跳过: %v", err)
	}
	// 去掉 # META 首行
	body := data
	if i := strings.IndexByte(string(data), '\n'); strings.HasPrefix(string(data), "# META") {
		body = data[i+1:]
	}
	out := runRescue(t, string(body))

	// 断言:输出里出现合成的 tool_use name=Bash
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("未合成 tool_use Bash block\n输出片段:\n%s", tail(out, 800))
	}
	// 断言:stop_reason 被改成 tool_use
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason 未改成 tool_use")
	}
	// 断言:command 参数还原(含 SDD_SKILL 片段)
	if !strings.Contains(out, "SDD_SKILL=") {
		t.Errorf("command 参数未正确还原")
	}
	// 断言:叙述文字保留(Task 8)
	if !strings.Contains(out, "Task 8") {
		t.Errorf("叙述文字未保留")
	}
	// 断言:partial_json 是合法 JSON(能解析回 map)
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

// 用真实样本 00072 驱动:text block 内容纯为 "court"(叙述为空),紧跟完整 AskUserQuestion。
// 期望:整个孤儿 text block 丢弃(剥完为空),AskUserQuestion 原样透传。
func TestRescueOrphanPrefixRealSample(t *testing.T) {
	path := os.ExpandEnv("$HOME/.claude-proxy/samples/20260914-111529-00072.sse")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("样本不存在,跳过: %v", err)
	}
	body := data
	if i := strings.IndexByte(string(data), '\n'); strings.HasPrefix(string(data), "# META") {
		body = data[i+1:]
	}
	out := runRescue(t, string(body))

	// 孤儿 court 不应作为 text_delta 泄漏出去
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
		if txt, ok := delta["text"].(string); ok && strings.TrimSpace(txt) == "court" {
			t.Errorf("孤儿 court text_delta 泄漏\n%s", line)
		}
	}
	// AskUserQuestion 原样透传
	if !strings.Contains(out, "AskUserQuestion") {
		t.Errorf("AskUserQuestion tool_use 丢失\n%s", tail(out, 400))
	}
	if !strings.Contains(out, "toolu_01PAE9uHNHXc1cy3CFerMiz8") {
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

// 用真实坏样本 00482 驱动:AskUserQuestion 的 questions 被双重编码,
// 救援后应还原成合法数组(input_json_delta 里 questions 是 array)。
func TestRescueInputFixRealSample(t *testing.T) {
	path := os.ExpandEnv("$HOME/.claude-proxy/samples/20260911-131049-00482.sse")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("样本不存在: %v", err)
	}
	body := data
	if i := strings.IndexByte(string(data), '\n'); strings.HasPrefix(string(data), "# META") {
		body = data[i+1:]
	}
	out := runRescue(t, string(body))

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
