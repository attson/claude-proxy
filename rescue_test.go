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
