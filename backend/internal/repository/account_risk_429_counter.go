package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var accountRisk429Script = redis.NewScript(`
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local window = tonumber(ARGV[2]) * 1000
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - window)
redis.call('ZADD', KEYS[1], 'NX', now, ARGV[1])
local count = redis.call('ZCARD', KEYS[1])
local cap = tonumber(ARGV[3])
if count > cap then redis.call('ZREMRANGEBYRANK', KEYS[1], 0, count - cap - 1) end
redis.call('PEXPIRE', KEYS[1], window)
return math.min(count, cap)
`)

type accountRisk429Counter struct{ rdb *redis.Client }

func NewAccountRisk429Counter(rdb *redis.Client) service.AccountRisk429Counter {
	return &accountRisk429Counter{rdb: rdb}
}

func (c *accountRisk429Counter) Increment(ctx context.Context, key, token string, window, cap int) (int64, error) {
	return accountRisk429Script.Run(ctx, c.rdb, []string{"account_risk_429:" + key}, token, window, cap).Int64()
}
