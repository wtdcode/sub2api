package repository

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// 账号级 TPM（tokens-per-minute）滑动窗口。
//
// 模型：预留-结算（reserve/settle）。
//   - 调度获取槽位时按估算值（× 校准系数）预留窗口额度，窗口超限则拒绝；
//   - usage 落账时用实际 token（按权重折算）以同一 request_id 结算，覆盖预留；
//   - 窗口 60 秒滚动剪枝：任何未结算的预留（请求失败/断开/泄漏）在 ≤60 秒内自愈，
//     因此无需在各 release/异常路径插入显式回收。
//
// 校准：结算时用 实际/估算 更新 EWMA（α=0.2，clamp [0.5, 3.0]），预留时在脚本内
// 直接乘上该系数——估算器的系统性偏差按账号自动收敛。
//
// 键使用 {accountID} hash tag 保证 Redis Cluster 下同槽。
const (
	tpmWindowKeyFmt = "concurrency:tpm:{%d}:win" // ZSET member=requestID score=unix秒
	tpmTokensKeyFmt = "concurrency:tpm:{%d}:tok" // HASH field=requestID value=tokens
	tpmCalKeyFmt    = "concurrency:tpm:{%d}:cal" // STRING 校准系数 ×1000 定点
	tpmWindowSecs   = 60
	tpmKeyTTLSecs   = 180
)

// tpmReserveScript KEYS: win, tok, cal; ARGV: requestID, estimateTokens, limit
// 返回 {admitted(0/1), windowSum, reservedTokens}
var tpmReserveScript = redis.NewScript(`
redis.replicate_commands()
local win, tok, cal = KEYS[1], KEYS[2], KEYS[3]
local reqID = ARGV[1]
local estimate = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local t = redis.call('TIME')
local now = tonumber(t[1])

-- 剪枝 60 秒外的条目
local expired = redis.call('ZRANGEBYSCORE', win, '-inf', now - ` + strconv.Itoa(tpmWindowSecs) + `)
if #expired > 0 then
  redis.call('ZREM', win, unpack(expired))
  redis.call('HDEL', tok, unpack(expired))
end

-- 当前窗口总量
local sum = 0
local vals = redis.call('HVALS', tok)
for i = 1, #vals do sum = sum + (tonumber(vals[i]) or 0) end

-- 校准后的预留量
local calv = tonumber(redis.call('GET', cal) or '1000')
local reserved = math.ceil(estimate * calv / 1000)
if reserved < 0 then reserved = 0 end

if limit > 0 and sum + reserved > limit and sum > 0 then
  return {0, sum, reserved}
end
-- sum==0 时即使单请求超限也放行（fail-open：空窗口不应饿死任何请求）

if reserved > 0 then
  redis.call('ZADD', win, now, reqID)
  redis.call('HSET', tok, reqID, reserved)
end
redis.call('EXPIRE', win, ` + strconv.Itoa(tpmKeyTTLSecs) + `)
redis.call('EXPIRE', tok, ` + strconv.Itoa(tpmKeyTTLSecs) + `)
return {1, sum + reserved, reserved}
`)

// tpmSettleScript KEYS: win, tok, cal; ARGV: requestID, actualTokens, estimateTokens
// 用实际值覆盖预留（member 相同则替换；预留已被剪枝或不存在则新增），并更新校准 EWMA。
var tpmSettleScript = redis.NewScript(`
redis.replicate_commands()
local win, tok, cal = KEYS[1], KEYS[2], KEYS[3]
local reqID = ARGV[1]
local actual = tonumber(ARGV[2])
local estimate = tonumber(ARGV[3])
local t = redis.call('TIME')
local now = tonumber(t[1])

local expired = redis.call('ZRANGEBYSCORE', win, '-inf', now - ` + strconv.Itoa(tpmWindowSecs) + `)
if #expired > 0 then
  redis.call('ZREM', win, unpack(expired))
  redis.call('HDEL', tok, unpack(expired))
end

if actual > 0 then
  redis.call('ZADD', win, now, reqID)
  redis.call('HSET', tok, reqID, actual)
  redis.call('EXPIRE', win, ` + strconv.Itoa(tpmKeyTTLSecs) + `)
  redis.call('EXPIRE', tok, ` + strconv.Itoa(tpmKeyTTLSecs) + `)
end

-- 校准 EWMA（α=0.2，×1000 定点，clamp [500, 3000]）
if estimate > 0 and actual > 0 then
  local ratio = actual * 1000 / estimate
  local old = tonumber(redis.call('GET', cal) or '1000')
  local new = math.floor(old * 0.8 + ratio * 0.2)
  if new < 500 then new = 500 end
  if new > 3000 then new = 3000 end
  redis.call('SET', cal, new, 'EX', 86400)
end
return 1
`)

func tpmKeys(accountID int64) []string {
	return []string{
		fmt.Sprintf(tpmWindowKeyFmt, accountID),
		fmt.Sprintf(tpmTokensKeyFmt, accountID),
		fmt.Sprintf(tpmCalKeyFmt, accountID),
	}
}

// ReserveAccountTPM 在账号 TPM 窗口中预留额度。limit<=0 时直接放行且不记账。
// 实现 service.AccountTPMCache。
func (c *concurrencyCache) ReserveAccountTPM(ctx context.Context, accountID int64, requestID string, estimateTokens int64, limit int64) (bool, error) {
	if limit <= 0 || c == nil || c.rdb == nil {
		return true, nil
	}
	res, err := tpmReserveScript.Run(ctx, c.rdb, tpmKeys(accountID), requestID, estimateTokens, limit).Slice()
	if err != nil {
		return true, err // fail-open：Redis 故障不应阻塞调度
	}
	if len(res) < 1 {
		return true, nil
	}
	admitted, _ := res[0].(int64)
	return admitted == 1, nil
}

// SettleAccountTPM 用实际 token 结算（覆盖预留）并更新校准系数。
func (c *concurrencyCache) SettleAccountTPM(ctx context.Context, accountID int64, requestID string, actualTokens int64, estimateTokens int64) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return tpmSettleScript.Run(ctx, c.rdb, tpmKeys(accountID), requestID, actualTokens, estimateTokens).Err()
}
