package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGetContextLength(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    int
	}{
		{"nil account", nil, 0},
		{"nil credentials", &Account{}, 0},
		{"missing key", &Account{Credentials: map[string]any{}}, 0},
		{"int value", &Account{Credentials: map[string]any{"context_length": 262144}}, 262144},
		{"float64 value (json decode)", &Account{Credentials: map[string]any{"context_length": float64(1048576)}}, 1048576},
		{"string value", &Account{Credentials: map[string]any{"context_length": "200000"}}, 200000},
		{"negative clamped", &Account{Credentials: map[string]any{"context_length": -1}}, 0},
		{"garbage string", &Account{Credentials: map[string]any{"context_length": "abc"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.GetContextLength())
		})
	}
}

func TestEstimateOpenAIChatPromptTokens(t *testing.T) {
	require.Equal(t, 0, EstimateOpenAIChatPromptTokens(nil))

	short := []byte(`{"model":"m","messages":[{"role":"user","content":"hello world"}]}`)
	est := EstimateOpenAIChatPromptTokens(short)
	require.Greater(t, est, 0)
	require.Less(t, est, 20)

	// ~400k ASCII chars → ~120k tokens (3.33 chars/token heuristic)
	long := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("word ", 80000) + `"}]}`)
	est = EstimateOpenAIChatPromptTokens(long)
	require.InDelta(t, 120000, est, 12000)

	// content parts + tools 也计入
	parts := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"aaaa bbbb cccc dddd"},{"type":"image_url","image_url":{"url":"http://x"}}]}],"tools":[{"type":"function","function":{"name":"f","description":"aaaa bbbb cccc dddd"}}]}`)
	require.Greater(t, EstimateOpenAIChatPromptTokens(parts), 8)
}

// Claude Code 形状：tool_result 嵌套 content 是上下文大头，必须被计入
// （上线首日曾因漏算它导致 /v1/messages 估算中位偏低到 0.59x）。
func TestEstimateOpenAIChatPromptTokens_AnthropicToolBlocks(t *testing.T) {
	bigToolOutput := strings.Repeat("line of file content here ", 4000) // ~104k chars → ~31k tokens
	body := []byte(`{"model":"m","system":[{"type":"text","text":"You are Claude Code."}],"messages":[
		{"role":"user","content":"read the file"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"I should read the file first to understand it."},
			{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a.txt"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"` + bigToolOutput + `"}]}]}
	]}`)
	est := EstimateOpenAIChatPromptTokens(body)
	require.Greater(t, est, 28000, "tool_result nested content must dominate the estimate")

	// 图片 block 不计入（防 base64 反向爆炸）
	img := []byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"` + strings.Repeat("A", 50000) + `"}}]}]}`)
	require.Less(t, EstimateOpenAIChatPromptTokens(img), 100)
}

func TestAccountContextLengthFits(t *testing.T) {
	small := &Account{Credentials: map[string]any{"context_length": 200000}}
	undeclared := &Account{Credentials: map[string]any{}}
	require.True(t, accountContextLengthFits(small, 0), "no estimate → always fits")
	require.True(t, accountContextLengthFits(small, 199999))
	require.False(t, accountContextLengthFits(small, 200001))
	require.True(t, accountContextLengthFits(undeclared, 1<<30), "undeclared → unlimited")
}

func newContextLengthTestService(accounts []Account) *OpenAIGatewayService {
	return newContextLengthTestServiceWithCache(accounts, schedulerTestConcurrencyCache{})
}

// LoadBatchEnabled=true 对齐生产默认（走 legacy 分层负载路径）。
func newContextLengthTestServiceWithCache(accounts []Account, cache schedulerTestConcurrencyCache) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(cache),
	}
}

func contextLengthTestAccounts() []Account {
	return []Account{
		{
			ID:          81001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 10,
			Priority:    0,
			Credentials: map[string]any{"context_length": 200000},
		},
		{
			ID:          81002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 10,
			Priority:    0,
			Credentials: map[string]any{"context_length": 1048576},
		},
	}
}

// 长请求：只有 1M 账号装得下，200k 账号必须被过滤。
func TestOpenAIScheduler_ContextLengthFiltersSmallWindow(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92001)
	svc := newContextLengthTestService(contextLengthTestAccounts())

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 300000)
	for i := 0; i < 10; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.Equal(t, int64(81002), selection.Account.ID, "long prompt must go to the 1M account")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

// 短请求：两台都装得下，但小窗口账号应当优先（把 1M 容量留给长请求）。
func TestOpenAIScheduler_ContextLengthPrefersSmallestSufficientWindow(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92002)
	svc := newContextLengthTestService(contextLengthTestAccounts())

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 1000)
	for i := 0; i < 10; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.Equal(t, int64(81001), selection.Account.ID, "short prompt should prefer the 200k account")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

// 无估算值（非 chat completions 入口等）：行为与改动前一致，两台皆可被选。
func TestOpenAIScheduler_NoEstimateKeepsLegacyBehavior(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92003)
	svc := newContextLengthTestService(contextLengthTestAccounts())

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection.Account)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 估算超过所有声明窗口（估算系统性偏高的场景）：fail-open 到最大窗口账号，
// 由上游做最终裁决，而不是整池拒绝。小窗口账号仍被过滤。
func TestOpenAIScheduler_ContextLengthOverestimateFailsOpenToLargestWindow(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92004)
	svc := newContextLengthTestService(contextLengthTestAccounts())

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 2000000)
	for i := 0; i < 5; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.Equal(t, int64(81002), selection.Account.ID, "over-estimate must fail open to the largest window, never the smaller one")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

func TestCapOpenAIEstimatedPromptTokensForPool(t *testing.T) {
	require.Equal(t, 0, capOpenAIEstimatedPromptTokensForPool(0, []int{200000}), "no estimate stays zero")
	require.Equal(t, 150000, capOpenAIEstimatedPromptTokensForPool(150000, []int{200000, 1048576}), "within max → unchanged")
	require.Equal(t, 1048576, capOpenAIEstimatedPromptTokensForPool(2000000, []int{200000, 1048576}), "above max → capped to max")
	require.Equal(t, 2000000, capOpenAIEstimatedPromptTokensForPool(2000000, []int{200000, 0}), "unlimited account present → no cap")
	require.Equal(t, 2000000, capOpenAIEstimatedPromptTokensForPool(2000000, nil), "empty pool → unchanged")
}

// 256k 负载打满（LoadRate=100）时，短请求应自动溢出到 1M 账号（分层不破坏并发调度）。
func TestOpenAIScheduler_ContextLengthSpillsWhenPreferredSaturated(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92005)
	svc := newContextLengthTestServiceWithCache(contextLengthTestAccounts(), schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			81001: {AccountID: 81001, LoadRate: 100, CurrentConcurrency: 10}, // 256k 打满
		},
		acquireResults: map[int64]bool{81001: false}, // 双保险：即便进入抢槽也失败
	})

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 1000)
	for i := 0; i < 5; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(81002), selection.Account.ID, "saturated 256k must spill to the 1M account")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

// 显式优先级压过窗口偏好：1M 账号 priority 更高（数字更小）时，短请求应优先去 1M；
// 平级场景的小窗口优先由 PrefersSmallestSufficientWindow 用例钉住。
func TestOpenAIScheduler_ExplicitPriorityOverridesWindowPreference(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92006)
	accounts := contextLengthTestAccounts()
	accounts[0].Priority = 50 // 256k
	accounts[1].Priority = 10 // 1M 显式更高优先级
	svc := newContextLengthTestService(accounts)

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 1000)
	for i := 0; i < 10; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		require.NoError(t, err)
		require.Equal(t, int64(81002), selection.Account.ID, "short prompt must follow explicit higher priority to the 1M account")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}

// 用户场景实锤：1M 并发打满、256k 空闲，来一个 900k 请求——绝不允许把装不下的
// 请求塞给 256k；只能在 1M 上排队等待（原有 wait-plan 逻辑）。
func TestOpenAIScheduler_LongPromptNeverSpillsToSmallWindowEvenWhenLargeIsSaturated(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92007)
	svc := newContextLengthTestServiceWithCache(contextLengthTestAccounts(), schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			81002: {AccountID: 81002, LoadRate: 100, CurrentConcurrency: 10}, // 1M 打满
			// 256k (81001) 空闲
		},
		acquireResults: map[int64]bool{81002: false}, // 1M 抢槽也失败
	})

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 900000)
	for i := 0; i < 5; i++ {
		selection, _, err := svc.SelectAccountWithScheduler(
			ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
		)
		if err != nil {
			continue // 无可用账号错误也是合法结果（走 failover/重试）
		}
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(81002), selection.Account.ID,
			"900k request must queue on the saturated 1M account, never leak to 256k")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}
}
