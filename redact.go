package main

import "regexp"

// 脱敏:代理内存里持有真实 token 用于转发,但**任何写盘内容都必须脱敏**。
var redactPatterns = []struct {
	re   *regexp.Regexp
	repl []byte
}{
	{regexp.MustCompile(`cr_[A-Za-z0-9_\-]+`), []byte("cr_REDACTED")},
	{regexp.MustCompile(`(?i)(authorization\s*:\s*)(bearer\s+)?\S+`), []byte("${1}${2}REDACTED")},
	{regexp.MustCompile(`(?i)(x-api-key\s*:\s*)\S+`), []byte("${1}REDACTED")},
	{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]+`), []byte("${1}REDACTED")},
	{regexp.MustCompile(`(?i)("(?:api_key|token|access_token|auth_token)"\s*:\s*")[^"]*(")`), []byte("${1}REDACTED${2}")},
}

// redact 对任意字节做脱敏,永不 panic。
func redact(data []byte) []byte {
	defer func() { _ = recover() }()
	for _, p := range redactPatterns {
		data = p.re.ReplaceAll(data, p.repl)
	}
	return data
}
