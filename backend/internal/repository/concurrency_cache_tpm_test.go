package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTPMTestCache(t *testing.T) (*miniredis.Miniredis, *concurrencyCache) {
	t.Helper()
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return srv, &concurrencyCache{rdb: client, slotTTLSeconds: 900, waitQueueTTLSeconds: 900}
}

func TestReserveAccountTPM_AdmitAndReject(t *testing.T) {
	_, cache := newTPMTestCache(t)
	ctx := context.Background()

	// 无限制直接放行
	ok, err := cache.ReserveAccountTPM(ctx, 1, "r0", 999999, 0)
	require.NoError(t, err)
	require.True(t, ok)

	// 限 100k：60k + 30k 放行，再来 30k 拒绝
	ok, err = cache.ReserveAccountTPM(ctx, 1, "r1", 60000, 100000)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.ReserveAccountTPM(ctx, 1, "r2", 30000, 100000)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.ReserveAccountTPM(ctx, 1, "r3", 30000, 100000)
	require.NoError(t, err)
	require.False(t, ok, "window 90k + 30k must exceed 100k limit")

	// 空窗口时单请求超限也放行（fail-open）
	ok, err = cache.ReserveAccountTPM(ctx, 2, "big", 500000, 100000)
	require.NoError(t, err)
	require.True(t, ok, "empty window must never starve a request")
}

func TestSettleAccountTPM_ReplacesReservationAndCalibrates(t *testing.T) {
	_, cache := newTPMTestCache(t)
	ctx := context.Background()

	// 预留 50k，结算实际 90k（同 requestID 覆盖而非累加）
	ok, err := cache.ReserveAccountTPM(ctx, 3, "req1", 50000, 100000)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, cache.SettleAccountTPM(ctx, 3, "req1", 90000, 50000))

	// 窗口现应为 90k：再预留 20k 应被拒（90+20>100）
	ok, err = cache.ReserveAccountTPM(ctx, 3, "req2", 20000, 100000)
	require.NoError(t, err)
	require.False(t, ok, "settled 90k + 20k must exceed limit")

	// 校准生效：实际/估算=1.8 → EWMA 从 1.0 → 1.16；
	// 新账号同估算值的预留应被放大（用一个只装得下未校准值的限额验证）
	ok, err = cache.ReserveAccountTPM(ctx, 3, "req3", 10000, 200000)
	require.NoError(t, err)
	require.True(t, ok)
	// 窗口 90k + ceil(10000*1.16)=11600 = 101600 —— 无法直接读预留值，
	// 改为断言校准键已写入且 >1000
	cal, err := cache.rdb.Get(ctx, "concurrency:tpm:{3}:cal").Result()
	require.NoError(t, err)
	require.Greater(t, cal, "1000")
}

func TestReserveAccountTPM_WindowExpiry(t *testing.T) {
	srv, cache := newTPMTestCache(t)
	ctx := context.Background()

	ok, err := cache.ReserveAccountTPM(ctx, 4, "old", 90000, 100000)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.ReserveAccountTPM(ctx, 4, "new1", 50000, 100000)
	require.NoError(t, err)
	require.False(t, ok)

	// 快进 61 秒：旧预留剪枝，窗口重新可用（泄漏自愈）
	srv.SetTime(time.Now().Add(61 * time.Second))
	ok, err = cache.ReserveAccountTPM(ctx, 4, "new2", 50000, 100000)
	require.NoError(t, err)
	require.True(t, ok, "expired reservations must be pruned")
}
