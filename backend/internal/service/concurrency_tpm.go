package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// AccountTPMCache 是 ConcurrencyCache 的可选能力：账号级 TPM 滑动窗口的
// 预留/结算。缓存实现不支持时 TPM 限制静默失效（fail-open）。
type AccountTPMCache interface {
	// ReserveAccountTPM 预留窗口额度；返回 false 表示窗口已满应拒绝。
	ReserveAccountTPM(ctx context.Context, accountID int64, requestID string, estimateTokens int64, limit int64) (bool, error)
	// SettleAccountTPM 用实际 token 结算并更新校准系数。
	SettleAccountTPM(ctx context.Context, accountID int64, requestID string, actualTokens int64, estimateTokens int64) error
	// GetAccountTPMUsageBatch 批量读取当前窗口用量（管理端展示）。
	GetAccountTPMUsageBatch(ctx context.Context, accountIDs []int64) (map[int64]int64, error)
}

// tpmCacheReadTokenWeight cache_read token 的计权（前缀缓存命中几乎不耗
// prefill 算力，不应惩罚粘性会话回头客）。
const tpmCacheReadTokenWeight = 0.1

// WeightedTPMTokens 按 TPM 记账规则折算实际用量：
// input + output + cache_creation 全额，cache_read 打 0.1 折。
func WeightedTPMTokens(inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) int64 {
	total := int64(inputTokens) + int64(outputTokens) + int64(cacheCreationTokens)
	total += int64(float64(cacheReadTokens) * tpmCacheReadTokenWeight)
	if total < 0 {
		return 0
	}
	return total
}

// tpmRequestIDFromContext 取请求关联键：预留与结算共用 ctxkey.RequestID，
// usage 采集路径已透传该值（handler usageRecordContext）。
func tpmRequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxkey.RequestID).(string)
	return id
}

// AcquireAccountSlotWithTPM 在并发槽之上叠加 TPM 窗口预留：
// 槽获取成功但 TPM 窗口已满时回滚槽并返回未获取（对调度器表现为"账号忙"，
// 溢出/排队走既有机器）。tpmLimit<=0 时行为与 AcquireAccountSlot 完全一致。
func (s *ConcurrencyService) AcquireAccountSlotWithTPM(ctx context.Context, accountID int64, maxConcurrency int, tpmLimit int, estimateTokens int) (*AcquireResult, error) {
	result, err := s.AcquireAccountSlot(ctx, accountID, maxConcurrency)
	if err != nil || result == nil || !result.Acquired {
		return result, err
	}
	if tpmLimit <= 0 || s == nil || s.cache == nil {
		return result, nil
	}
	tpmCache, ok := s.cache.(AccountTPMCache)
	if !ok {
		return result, nil
	}
	requestID := tpmRequestIDFromContext(ctx)
	if requestID == "" {
		requestID = generateRequestID()
	}
	admitted, tpmErr := tpmCache.ReserveAccountTPM(ctx, accountID, requestID, int64(estimateTokens), int64(tpmLimit))
	if tpmErr != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: TPM reserve failed for account %d (fail-open): %v", accountID, tpmErr)
		return result, nil
	}
	if admitted {
		return result, nil
	}
	if result.ReleaseFunc != nil {
		result.ReleaseFunc()
	}
	return &AcquireResult{Acquired: false, ReleaseFunc: nil}, nil
}

// SettleAccountTPM 用实际 token 结算窗口（usage 落账路径调用）。
// 预留缺失（请求前未限流/预留已过期）时仍记入实际用量，保证窗口真实。
func (s *ConcurrencyService) SettleAccountTPM(ctx context.Context, accountID int64, actualTokens int64, estimateTokens int64) {
	if s == nil || s.cache == nil || actualTokens <= 0 {
		return
	}
	tpmCache, ok := s.cache.(AccountTPMCache)
	if !ok {
		return
	}
	requestID := tpmRequestIDFromContext(ctx)
	if requestID == "" {
		requestID = generateRequestID()
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tpmCache.SettleAccountTPM(bgCtx, accountID, requestID, actualTokens, estimateTokens); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: TPM settle failed for account %d: %v", accountID, err)
	}
}

// GetAccountTPMUsageBatch 返回各账号当前 60 秒窗口内的 token 用量。
// 缓存实现不支持 TPM 时返回空 map。
func (s *ConcurrencyService) GetAccountTPMUsageBatch(ctx context.Context, accountIDs []int64) map[int64]int64 {
	if s == nil || s.cache == nil || len(accountIDs) == 0 {
		return nil
	}
	tpmCache, ok := s.cache.(AccountTPMCache)
	if !ok {
		return nil
	}
	usage, err := tpmCache.GetAccountTPMUsageBatch(ctx, accountIDs)
	if err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: TPM usage batch read failed: %v", err)
		return nil
	}
	return usage
}
