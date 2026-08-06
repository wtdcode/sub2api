package service

import (
	"encoding/json"
	"strconv"
	"strings"
)

// accountContextLengthKey 是 OpenAI 兼容上游账号在 credentials 中声明上下文窗口
// （单位 token）的可选配置键。0 或缺省表示未声明（视为不限制）。
const accountContextLengthKey = "context_length"

// GetContextLength 返回账号声明的上下文窗口（token 数）。
// 未声明、非法或非正值一律返回 0，调度器将其视为"无长度约束"。
func (a *Account) GetContextLength() int {
	if a == nil || a.Credentials == nil {
		return 0
	}
	raw, ok := a.Credentials[accountContextLengthKey]
	if !ok || raw == nil {
		return 0
	}
	length := parseAccountContextLength(raw)
	if length < 0 {
		return 0
	}
	return length
}

func parseAccountContextLength(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return 0
}
