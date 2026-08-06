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
		content := message.Get("content")
		switch {
		case content.Type == gjson.String:
			total += estimateTokensForText(content.String())
		case content.IsArray():
			content.ForEach(func(_, part gjson.Result) bool {
				if t := strings.TrimSpace(part.Get("text").String()); t != "" {
					total += estimateTokensForText(t)
				}
				return true
			})
		}
		if calls := message.Get("tool_calls"); calls.Exists() {
			total += estimateTokensForText(calls.Raw)
		}
		return true
	})

	// Responses API 形状：instructions + input（字符串或 item 列表）。
	if instr := gjson.GetBytes(body, "instructions"); instr.Type == gjson.String {
		total += estimateTokensForText(instr.String())
	}
	input := gjson.GetBytes(body, "input")
	switch {
	case input.Type == gjson.String:
		total += estimateTokensForText(input.String())
	case input.IsArray():
		input.ForEach(func(_, item gjson.Result) bool {
			total += openAIChatMessageTokenOverhead
			content := item.Get("content")
			switch {
			case content.Type == gjson.String:
				total += estimateTokensForText(content.String())
			case content.IsArray():
				content.ForEach(func(_, part gjson.Result) bool {
					if t := strings.TrimSpace(part.Get("text").String()); t != "" {
						total += estimateTokensForText(t)
					}
					return true
				})
			}
			return true
		})
	}

	// Anthropic 形状：顶层 system（字符串或 block 列表）；messages 复用上面的通用遍历。
	system := gjson.GetBytes(body, "system")
	switch {
	case system.Type == gjson.String:
		total += estimateTokensForText(system.String())
	case system.IsArray():
		system.ForEach(func(_, part gjson.Result) bool {
			if t := strings.TrimSpace(part.Get("text").String()); t != "" {
				total += estimateTokensForText(t)
			}
			return true
		})
	}

	// tools/functions 定义随每次请求进入上下文，长 schema 不可忽略。
	if tools := gjson.GetBytes(body, "tools"); tools.Exists() {
		total += estimateTokensForText(tools.Raw)
	}

	if total < 0 {
		return 0
	}
	return total
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

// partitionOpenAICandidatesByContextLength 将候选按声明的上下文窗口升序分层：
// 窗口小且够用的账号排前（把大窗口容量留给长请求），未声明窗口的账号排最后。
// 层内维持调用方的原有顺序，由上层继续做 score/priority 排序。
// estimatedTokens<=0 时不分层，原样返回单层。
func partitionOpenAICandidatesByContextLength(estimatedTokens int, pool []openAIAccountCandidateScore) [][]openAIAccountCandidateScore {
	if estimatedTokens <= 0 || len(pool) == 0 {
		return [][]openAIAccountCandidateScore{pool}
	}
	byLimit := make(map[int][]openAIAccountCandidateScore)
	for _, candidate := range pool {
		limit := candidate.account.GetContextLength()
		if limit <= 0 {
			limit = int(^uint(0) >> 1) // 未声明 → 视为最大窗口，排最后一层
		}
		byLimit[limit] = append(byLimit[limit], candidate)
	}
	if len(byLimit) <= 1 {
		return [][]openAIAccountCandidateScore{pool}
	}
	limits := make([]int, 0, len(byLimit))
	for limit := range byLimit {
		limits = append(limits, limit)
	}
	sort.Ints(limits)
	tiers := make([][]openAIAccountCandidateScore, 0, len(limits))
	for _, limit := range limits {
		tiers = append(tiers, byLimit[limit])
	}
	return tiers
}
