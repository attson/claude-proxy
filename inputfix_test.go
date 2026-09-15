package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实退化:AskUserQuestion 的 questions 被双重编码成字符串。
func TestFixDoubleEncodedQuestions(t *testing.T) {
	// 模拟样本 00482 的坏形态:questions 值是 JSON 数组的字符串
	inner := `[{"question":"q1","header":"h1","options":[{"label":"a","description":"d"}]}]`
	bad, _ := json.Marshal(map[string]interface{}{"questions": inner})

	out, fixed := fixToolInput("AskUserQuestion", bad)
	if !fixed {
		t.Fatalf("未修复双重编码的 questions")
	}
	// 修复后 questions 应是数组
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("修复后 JSON 非法: %v", err)
	}
	if _, ok := m["questions"].([]interface{}); !ok {
		t.Fatalf("questions 未还原成数组, 实际类型 %T", m["questions"])
	}
}

// 正常 AskUserQuestion(questions 本就是数组)不应被改动。
func TestFixNormalQuestionsUntouched(t *testing.T) {
	good := []byte(`{"questions":[{"question":"q","header":"h","options":[]}]}`)
	out, fixed := fixToolInput("AskUserQuestion", good)
	if fixed {
		t.Errorf("正常 questions 被误改")
	}
	if string(out) != string(good) {
		t.Errorf("正常 input 被改写")
	}
}

// 其他工具(如 Bash)即使 command 是 JSON 字符串也不碰(不在已知坏字段表)。
func TestFixOtherToolUntouched(t *testing.T) {
	// Bash 的 command 恰好是一段 JSON 文本 —— 本就该是字符串,绝不能解一层
	in := []byte(`{"command":"[1,2,3]"}`)
	out, fixed := fixToolInput("Bash", in)
	if fixed {
		t.Errorf("Bash 的 command 被误解一层")
	}
	if string(out) != string(in) {
		t.Errorf("Bash input 被改写")
	}
}

// 坏 JSON 不 panic,原样返回。
func TestFixMalformedInput(t *testing.T) {
	in := []byte(`{"questions": not-json`)
	out, fixed := fixToolInput("AskUserQuestion", in)
	if fixed {
		t.Errorf("非法 JSON 不应被标记为已修复")
	}
	if string(out) != string(in) {
		t.Errorf("非法 input 应原样返回")
	}
}

// —— cleanBadUnicodeEscapes 行为契约护栏 ——
// 这些测试固化当前实现的既定契约,防止后续改动越界:
//   1) hex 齐全、仅被空白分隔的坏转义 → 无损清洗回合法转义;
//   2) hex 字符本身丢失(不足 4 位)→ 保留坏 \u,绝不猜、不借相邻转义的 hex,交上层 fail-open。
// 注:契约 2 覆盖真实样本 20260915-100153-00034 里 hex 丢字的那类退化——它本就不可无损还原。

// 契约 1:hex 齐全、被空白分隔的坏转义,应无损清洗成合法转义。
// 输出仍是 \uXXXX 字面串;用「把输出当 JSON 字符串解码后应得预期字符」断言,避免手写脆弱字面串。
func TestCleanBadUnicodeEscapesSpaceOnly(t *testing.T) {
	decodeJSONStr := func(t *testing.T, body string) string {
		t.Helper()
		var s string
		if err := json.Unmarshal([]byte(`"`+body+`"`), &s); err != nil {
			t.Fatalf("清洗后仍不是合法 JSON 字符串: %v (body=%q)", err, body)
		}
		return s
	}
	cases := []struct {
		name string
		in   string // 原始文本(原始字符串字面量,\u 是字面反斜杠+u)
		want string // 把 cleanBadUnicodeEscapes 输出当 JSON 字符串解码后的预期
	}{
		{"空格在 hex 之间_后接汉字", `\u7ed 9定`, "给定"},
		{"空格在 hex 之间_后接普通字符", `\u7 ed9x`, "给x"},
		{"已合法不动", `中文`, "中文"},
		{"多个空格散在 hex 间", `\u4 e 2 d`, "中"},
		{"非转义处的空格不受影响", `hello world`, "hello world"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeJSONStr(t, cleanBadUnicodeEscapes(c.in))
			if got != c.want {
				t.Errorf("cleanBadUnicodeEscapes(%q) 解码后 = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// 契约 2:救不了的坏形态一律保留坏 \u(输出仍非合法 JSON 字符串体),交上层 fail-open,不越界不 panic。
func TestCleanBadUnicodeEscapesFailOpen(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"只有反斜杠u", `\u`},
		{"hex不足且到末尾", `\u12`},
		{"遇引号break_hex不足", `\u12"tail`},
		{"空格加引号_凑不满", `\u1 2"`},
		// hex 只剩 3 位(第 4 位字符丢失),后面紧跟另一个合法转义——绝不能借它的 hex 凑数。
		{"hex丢一位_紧贴合法转义", `\u7 ee定`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := cleanBadUnicodeEscapes(c.in) // 不 panic
			var s string
			if json.Unmarshal([]byte(`"`+out+`"`), &s) == nil {
				t.Errorf("不可无损还原的坏转义不应被清洗成合法 JSON: in=%q out=%q", c.in, out)
			}
		})
	}
}

// 契约 2 补充:hex 丢字时不得借相邻合法转义的 hex 拼凑,相邻字符必须原样保留。
func TestCleanBadUnicodeEscapesNoBorrow(t *testing.T) {
	// `\u7 ee定`:\u7 ee 只有 3 个 hex,紧跟合法转义 定(定)。
	// 正确行为:保留坏 \u,不吞并 定。
	out := cleanBadUnicodeEscapes(`\u7 ee定`)
	if !strings.Contains(out, `定`) {
		t.Errorf("相邻的合法转义 \\u5b9a 被吞并/破坏: out=%q", out)
	}
}

// 端到端契约 1:questions 被双重编码,内层含 hex 齐全的空格污染 \u,应清洗后解一层成功。
func TestFixDoubleEncodedWithSpaceOnlyBadUnicode(t *testing.T) {
	// \u7 ed9=给,\u5b9 a=定,hex 都齐全。
	inner := `[{"question":"\u7 ed9\u5b9 a?","header":"h","options":[{"label":"a","description":"d"}]}]`
	bad, _ := json.Marshal(map[string]interface{}{"questions": inner})

	out, fixed := fixToolInput("AskUserQuestion", bad)
	if !fixed {
		t.Fatalf("hex 齐全的空格污染 questions 未被修复")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("修复后 JSON 非法: %v", err)
	}
	arr, ok := m["questions"].([]interface{})
	if !ok {
		t.Fatalf("questions 未还原成数组, 实际类型 %T", m["questions"])
	}
	if len(arr) != 1 {
		t.Fatalf("questions 数组长度 = %d, want 1", len(arr))
	}
}

// 端到端契约 2:内层含 hex 丢字的坏转义,不可无损还原,fixToolInput 应 fail-open(不标记 fixed、原样返回)。
func TestFixDoubleEncodedWithLostHexFailOpen(t *testing.T) {
	// \u7 ee 只有 3 个 hex(第 4 位丢失),紧跟 定。真实样本 00034 的形态。
	inner := `[{"question":"未\u7 ee定?","header":"h","options":[{"label":"a","description":"d"}]}]`
	bad, _ := json.Marshal(map[string]interface{}{"questions": inner})

	out, fixed := fixToolInput("AskUserQuestion", bad)
	if fixed {
		t.Errorf("hex 丢字不可无损还原,不应标记为已修复(应 fail-open)")
	}
	if string(out) != string(bad) {
		t.Errorf("fail-open 时应原样返回 input")
	}
}
