package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// tpmTestConcurrencyCache 给调度测试桩叠加 AccountTPMCache 能力。
type tpmTestConcurrencyCache struct {
	schedulerTestConcurrencyCache
	tpmAdmit map[int64]bool // 缺省 true
	settled  *[]int64
}

func (c tpmTestConcurrencyCache) ReserveAccountTPM(_ context.Context, accountID int64, _ string, _ int64, _ int64) (bool, error) {
	if c.tpmAdmit != nil {
		if admit, ok := c.tpmAdmit[accountID]; ok {
			return admit, nil
		}
	}
	return true, nil
}

func (c tpmTestConcurrencyCache) SettleAccountTPM(_ context.Context, accountID int64, _ string, _ int64, _ int64) error {
	if c.settled != nil {
		*c.settled = append(*c.settled, accountID)
	}
	return nil
}

func TestWeightedTPMTokens(t *testing.T) {
	require.Equal(t, int64(0), WeightedTPMTokens(0, 0, 0, 0))
	require.Equal(t, int64(300), WeightedTPMTokens(100, 100, 100, 0))
	require.Equal(t, int64(110), WeightedTPMTokens(100, 0, 0, 100), "cache_read at 0.1 weight")
}

// AcquireAccountSlotWithTPM：TPM 拒绝时必须回滚并发槽。
func TestAcquireAccountSlotWithTPM_RollsBackSlotOnReject(t *testing.T) {
	var released []int64
	svc := NewConcurrencyService(tpmTestConcurrencyCache{
		schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{releasedIDs: &released},
		tpmAdmit:                      map[int64]bool{7: false},
	})
	result, err := svc.AcquireAccountSlotWithTPM(context.Background(), 7, 10, 100000, 5000)
	require.NoError(t, err)
	require.False(t, result.Acquired)
	require.Equal(t, []int64{7}, released, "slot must be released when TPM rejects")

	// tpmLimit=0 → 行为与普通获取一致
	result, err = svc.AcquireAccountSlotWithTPM(context.Background(), 7, 10, 0, 5000)
	require.NoError(t, err)
	require.True(t, result.Acquired)
	result.ReleaseFunc()
}

// 调度层：TPM 打满的小窗口账号自动溢出到另一个账号。
func TestOpenAIScheduler_TPMSaturatedAccountSpills(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92008)
	accounts := contextLengthTestAccounts()
	accounts[0].Credentials["tpm_limit"] = 100000 // 256k 账号限 100k TPM
	accounts[1].Credentials["tpm_limit"] = 2000000
	cfg := newContextLengthTestServiceWithCache(accounts, schedulerTestConcurrencyCache{})
	cfg.concurrencyService = NewConcurrencyService(tpmTestConcurrencyCache{
		tpmAdmit: map[int64]bool{81001: false}, // 256k 的 TPM 窗口已满
	})

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 1000)
	for i := 0; i < 5; i++ {
		selection, _, err := cfg.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(81002), selection.Account.ID, "TPM-saturated 256k must spill to the 1M account")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}
