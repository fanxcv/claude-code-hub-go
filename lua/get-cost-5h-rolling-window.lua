
local key = KEYS[1]
local now_ms = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])  -- 5 hours = 18000000 ms

-- 1. 清理过期记录
redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)

-- 2. 计算窗口内总消费
local records = redis.call('ZRANGE', key, 0, -1)
local total = 0
for _, record in ipairs(records) do
  local cost_str = string.match(record, '.*:(.+)')
  if cost_str then
    total = total + tonumber(cost_str)
  end
end

return tostring(total)
