package service

// accountTPMLimitKey 是 OpenAI 兼容上游账号在 credentials 中声明每分钟 token
// 预算（TPM）的可选配置键。0 或缺省表示不限制。
// 计入规则：input + output + cache_creation 全额，cache_read 按 0.1 权重
// （前缀缓存命中几乎不耗 prefill 算力，不应惩罚粘性会话的回头客）。
const accountTPMLimitKey = "tpm_limit"

// GetTPMLimit 返回账号声明的每分钟 token 预算。
// 未声明、非法或非正值一律返回 0，调度器将其视为"无 TPM 约束"。
func (a *Account) GetTPMLimit() int {
	if a == nil || a.Credentials == nil {
		return 0
	}
	raw, ok := a.Credentials[accountTPMLimitKey]
	if !ok || raw == nil {
		return 0
	}
	limit := parseAccountContextLength(raw)
	if limit < 0 {
		return 0
	}
	return limit
}
