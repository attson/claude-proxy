package main

import (
	"encoding/json"
	"strings"
)

// isHex 判断字符是否为十六进制数字。
func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// cleanBadUnicodeEscapes 修复被杂散字符污染的 \u 转义:
// 退化时 \uXXXX 中偶发插入杂散字符(如空格),变成 \u7 ee7(应为 继)。
// 规则:遇到 \u 后,从后续字符里跳过非 hex 字符、收集 4 个 hex 拼回 \uXXXX。
// 收集不到 4 个 hex 则原样保留该处(交给上层 fail-open)。
func cleanBadUnicodeEscapes(s string) string {
	var b strings.Builder
	i := 0
	n := len(s)
	for i < n {
		// 处理转义:遇到 \u 且后 4 位不是合法 hex 时尝试清洗
		if s[i] == '\\' && i+1 < n && s[i+1] == 'u' {
			// 已经合法?直接透传
			if i+6 <= n && isHex(s[i+2]) && isHex(s[i+3]) && isHex(s[i+4]) && isHex(s[i+5]) {
				b.WriteString(s[i : i+6])
				i += 6
				continue
			}
			// 从 i+2 起跳过非 hex,收集 4 个 hex
			hex := make([]byte, 0, 4)
			j := i + 2
			for j < n && len(hex) < 4 {
				c := s[j]
				if isHex(c) {
					hex = append(hex, c)
				} else if c == '\\' || c == '"' {
					break // 碰到新转义/字符串结束,放弃
				}
				j++
			}
			if len(hex) == 4 {
				b.WriteString("\\u")
				b.Write(hex)
				i = j
				continue
			}
			// 收集失败:原样保留 \u,继续(上层会解析失败并 fail-open)
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// 已知会被网关"双重编码成字符串"的 tool_use input 字段:工具名 -> 字段名列表。
// 这些字段按 schema 应是 array/object,但退化时被序列化成了 JSON 字符串。
// 仅对这些已知字段做"解一层"修复,避免误伤本就该是 JSON 字符串的字段。
var knownDoubleEncoded = map[string][]string{
	"AskUserQuestion": {"questions"},
}

// fixToolInput 检查并修复一个 tool_use 的 input JSON。
// 若该工具的已知字段的值是"看起来是 array/object 的 JSON 字符串",解一层还原。
// 返回 (修复后的 JSON 字节, 是否发生了修复)。任何异常返回原样、false(fail-open)。
func fixToolInput(toolName string, raw []byte) (out []byte, fixed bool) {
	defer func() {
		if r := recover(); r != nil {
			out, fixed = raw, false
		}
	}()

	fields, ok := knownDoubleEncoded[toolName]
	if !ok {
		return raw, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}

	changed := false
	for _, f := range fields {
		s, isStr := m[f].(string)
		if !isStr {
			continue
		}
		// 值是字符串:尝试把它解析成 array/object。成功则替换(解一层)。
		var v interface{}
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			// 内层 JSON 可能含被污染的 \u 转义,清洗后重试一次
			cleaned := cleanBadUnicodeEscapes(s)
			if cleaned == s || json.Unmarshal([]byte(cleaned), &v) != nil {
				continue
			}
		}
		switch v.(type) {
		case []interface{}, map[string]interface{}:
			m[f] = v
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	nb, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return nb, true
}
