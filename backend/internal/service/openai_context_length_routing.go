package service

import (
	"context"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// perMessageTokenOverhead 每条消息的封装开销（role、分隔符等）近似值，
// 与 openai_gateway_count_tokens.go 的 item overhead 取齐。
const openAIChatMessageTokenOverhead = 3

// EstimateOpenAIChatPromptTokens 对 /v1/chat/completions 原始请求体做快速
// prompt 长度估算（启发式，非精确 tokenizer），用于调度前的上下文窗口过滤。
// 估算覆盖 messages 文本内容（含多段 content parts 与 tool 消息）和 tools 定义。
// 返回 0 表示无法估算。
func EstimateOpenAIChatPromptTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	total := 0

	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		total += openAIChatMessageTokenOverhead
		total += estimateOpenAIContentTokens(message.Get("content"), 0)
		if calls := message.Get("tool_calls"); calls.Exists() {
			total += estimateRoutingTextTokens(calls.Raw)
		}
		return true
	})

	// Responses API 形状：instructions + input（字符串或 item 列表）。
	if instr := gjson.GetBytes(body, "instructions"); instr.Type == gjson.String {
		total += estimateRoutingTextTokens(instr.String())
	}
	if input := gjson.GetBytes(body, "input"); input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			total += openAIChatMessageTokenOverhead
			total += estimateOpenAIContentTokens(item.Get("content"), 0)
			// function_call / function_call_output item 的文本字段
			total += estimateRoutingTextTokens(item.Get("arguments").String())
			total += estimateOpenAIContentTokens(item.Get("output"), 0)
			return true
		})
	} else if input.Type == gjson.String {
		total += estimateRoutingTextTokens(input.String())
	}

	// Anthropic 形状：顶层 system（字符串或 block 列表）；messages 复用上面的通用遍历。
	total += estimateOpenAIContentTokens(gjson.GetBytes(body, "system"), 0)

	// tools/functions 定义随每次请求进入上下文，长 schema 不可忽略。
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() {
		total += estimateRoutingTextTokens(tools.Raw)
	}

	if total < 0 {
		return 0
	}
	return total
}

// estimateOpenAIContentTokens 递归估算 content 值（字符串或 block 数组）的 token 数。
// 覆盖所有承载文本的 block：text/thinking、tool_use.input、tool_result 的嵌套
// content（Claude Code 上下文大头——上线首日曾因漏算它导致 /v1/messages 估算
// 中位偏低到 0.59x）。图片类 block 跳过（计 raw 会被 base64 反向爆掉）。
// depth 限制防御恶意深嵌套。
func estimateOpenAIContentTokens(content gjson.Result, depth int) int {
	if depth > 4 {
		return 0
	}
	switch {
	case content.Type == gjson.String:
		return estimateRoutingTextTokens(content.String())
	case content.IsArray():
		total := 0
		content.ForEach(func(_, part gjson.Result) bool {
			total += estimateOpenAIContentPartTokens(part, depth)
			return true
		})
		return total
	}
	return 0
}

func estimateOpenAIContentPartTokens(part gjson.Result, depth int) int {
	switch part.Get("type").String() {
	case "image", "image_url", "input_image", "document":
		return 0
	case "tool_use", "server_tool_use":
		return estimateRoutingTextTokens(part.Get("name").String()) +
			estimateRoutingTextTokens(part.Get("input").Raw)
	case "tool_result", "web_search_tool_result":
		return estimateOpenAIContentTokens(part.Get("content"), depth+1)
	}
	total := 0
	if t := strings.TrimSpace(part.Get("text").String()); t != "" {
		total += estimateRoutingTextTokens(t)
	}
	if t := strings.TrimSpace(part.Get("thinking").String()); t != "" {
		total += estimateRoutingTextTokens(t)
	}
	if total == 0 {
		if nested := part.Get("content"); nested.Exists() {
			total += estimateOpenAIContentTokens(nested, depth+1)
		}
	}
	return total
}


// estimateRoutingTextTokens 路由估算专用的文本 token 启发式。
// 与共享的 estimateTokensForText 相比 ASCII 比率更紧(3.33 字符/token vs 4)：
// 生产数据显示代码类英文文本按 4 估算偏低 ~17%(usage 导出 p90=0.83)。
// CJK 仍按 1 token/rune(对 DeepSeek 词表约 +30% 高估, 安全方向)。
func estimateRoutingTextTokens(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	ascii := 0
	for _, r := range runes {
		if r <= 0x7f {
			ascii++
		}
	}
	if float64(ascii)/float64(len(runes)) >= 0.8 {
		return (len(runes)*3 + 9) / 10
	}
	return len(runes)
}

type openAIEstimatedPromptTokensCtxKey struct{}

// WithOpenAIEstimatedPromptTokens 将请求的 prompt 长度估算值挂到 ctx，
// 供调度器（高级与 legacy 两条路径）做上下文窗口过滤与偏好排序。
func WithOpenAIEstimatedPromptTokens(ctx context.Context, tokens int) context.Context {
	if tokens <= 0 {
		return ctx
	}
	return context.WithValue(ctx, openAIEstimatedPromptTokensCtxKey{}, tokens)
}

// OpenAIEstimatedPromptTokensFromContext 导出版读取函数，供 handler 层在派生
// 新 ctx（如异步 usage 记录）时跨 ctx 透传估算值。
func OpenAIEstimatedPromptTokensFromContext(ctx context.Context) int {
	return openAIEstimatedPromptTokensFromContext(ctx)
}

func openAIEstimatedPromptTokensFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if v, ok := ctx.Value(openAIEstimatedPromptTokensCtxKey{}).(int); ok && v > 0 {
		return v
	}
	return 0
}

// accountContextLengthFits 判断账号声明的上下文窗口能否容纳估算的 prompt 长度。
// 未声明（0）视为不限制。
func accountContextLengthFits(account *Account, estimatedTokens int) bool {
	if estimatedTokens <= 0 {
		return true
	}
	limit := account.GetContextLength()
	return limit <= 0 || estimatedTokens <= limit
}

// capOpenAIEstimatedPromptTokensForPool 把估算值截断到候选池的最大声明窗口。
// 估算是启发式且系统性偏高的：当它高于池内所有窗口时不应整池拒绝（fail-open），
// 而是按最大窗口处理——小窗口照常被过滤，最大窗口账号存活，由上游做最终裁决。
// 池内存在未声明窗口（0=不限）的账号时无需截断（该账号本就不会被过滤）。
func capOpenAIEstimatedPromptTokensForPool(est int, contextLengths []int) int {
	if est <= 0 || len(contextLengths) == 0 {
		return est
	}
	maxLimit := 0
	for _, limit := range contextLengths {
		if limit <= 0 {
			return est // 存在不限窗口的账号，无需截断
		}
		if limit > maxLimit {
			maxLimit = limit
		}
	}
	if maxLimit > 0 && est > maxLimit {
		return maxLimit
	}
	return est
}

// openAIContextLengthSortValue 返回账号用于窗口升序排序的键：
// 未声明（0）视为最大窗口排最后。
func openAIContextLengthSortValue(account *Account) int {
	limit := account.GetContextLength()
	if limit <= 0 {
		return int(^uint(0) >> 1)
	}
	return limit
}

// partitionOpenAICandidatesByContextLength 将候选按（调度优先级升序, 声明窗口升序）
// 分层：显式设置的账号优先级（数字小者先）压过窗口偏好——管理员意图第一；同优先级
// 内窗口小且够用的账号排前（把大窗口容量留给长请求），未声明窗口的排该优先级组最后。
// 层内维持调用方的原有顺序，由上层继续做 score 排序。
// estimatedTokens<=0 时不分层，原样返回单层。
func partitionOpenAICandidatesByContextLength(estimatedTokens int, pool []openAIAccountCandidateScore) [][]openAIAccountCandidateScore {
	if estimatedTokens <= 0 || len(pool) == 0 {
		return [][]openAIAccountCandidateScore{pool}
	}
	type tierKey struct {
		priority int
		limit    int
	}
	byKey := make(map[tierKey][]openAIAccountCandidateScore)
	for _, candidate := range pool {
		key := tierKey{
			priority: candidate.priority,
			limit:    openAIContextLengthSortValue(candidate.account),
		}
		byKey[key] = append(byKey[key], candidate)
	}
	if len(byKey) <= 1 {
		return [][]openAIAccountCandidateScore{pool}
	}
	keys := make([]tierKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].priority != keys[j].priority {
			return keys[i].priority < keys[j].priority
		}
		return keys[i].limit < keys[j].limit
	})
	tiers := make([][]openAIAccountCandidateScore, 0, len(keys))
	for _, key := range keys {
		tiers = append(tiers, byKey[key])
	}
	return tiers
}
