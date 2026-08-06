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

	// ~400k ASCII chars → ~100k tokens (4 chars/token heuristic)
	long := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("word ", 80000) + `"}]}`)
	est = EstimateOpenAIChatPromptTokens(long)
	require.InDelta(t, 100000, est, 10000)

	// content parts + tools 也计入
	parts := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"aaaa bbbb cccc dddd"},{"type":"image_url","image_url":{"url":"http://x"}}]}],"tools":[{"type":"function","function":{"name":"f","description":"aaaa bbbb cccc dddd"}}]}`)
	require.Greater(t, EstimateOpenAIChatPromptTokens(parts), 8)
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
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
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

// 请求超过所有声明窗口：无可用账号错误，而不是硬发给装不下的上游。
func TestOpenAIScheduler_ContextLengthExhaustsAllAccounts(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(92004)
	svc := newContextLengthTestService(contextLengthTestAccounts())

	ctx := WithOpenAIEstimatedPromptTokens(context.Background(), 2000000)
	_, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "deepseek-v4-flash", nil, OpenAIUpstreamTransportAny, false,
	)
	require.Error(t, err)
}
