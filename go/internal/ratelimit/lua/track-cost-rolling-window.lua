
local key = KEYS[1]
local cost = tonumber(ARGV[1])
local now_ms = tonumber(ARGV[2])
local window_ms = tonumber(ARGV[3])
local request_id = ARGV[4]
local ttl_seconds = tonumber(ARGV[5])

if not cost or not now_ms or not window_ms or not ttl_seconds then
  return redis.error_reply('invalid rolling cost arguments')
end

-- 1. 清理窗口外的消费记录
redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)

-- 2. 添加当前消费记录（member = timestamp:cost 或 timestamp:requestId:cost，便于调试和追踪）
local member
if request_id and request_id ~= '' then
  member = now_ms .. ':' .. request_id .. ':' .. cost
else
  member = now_ms .. ':' .. cost
end
redis.call('ZADD', key, now_ms, member)

-- 3. 恢复兜底 TTL，允许写路径修复缺失 TTL 的合法或脏 ZSET
redis.call('EXPIRE', key, ttl_seconds)

return 1
