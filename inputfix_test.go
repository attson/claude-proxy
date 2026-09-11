package main

import (
	"encoding/json"
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
