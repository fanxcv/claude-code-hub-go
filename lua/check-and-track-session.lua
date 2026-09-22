local provider_key = KEYS[1]
local ref_key = KEYS[2]
local member = ARGV[1]
local limit = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4]) or 300000

-- Guard against invalid TTL (prevents clearing all sessions)
if ttl <= 0 then
  ttl = 300000
end

-- 1. Cleanup expired attempts (TTL window ago)
local cutoff = now - ttl
local expired_members = redis.call('ZRANGEBYSCORE', provider_key, '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', provider_key, '-inf', cutoff)
for _, expired_member in ipairs(expired_members) do
  redis.call('HDEL', ref_key, expired_member)
end

-- 2. Count in-flight attempts.
-- 成员是**每次在飞尝试的 token**（不是会话身份），故 ZCARD 就是「在飞尝试数」。
local current_count = redis.call('ZCARD', provider_key)

-- 3. Check limit.
-- **不豁免任何成员**：每个在飞尝试各占一个额度。旧实现豁免「已在集合里的成员」，
-- 那是按会话计数时代的产物——它让同一会话的并行请求、竞速与重叠尝试只占一个额度，
-- 从而绕过渠道并发上限（用户 2026-09-22 的裁决是「在飞请求数（每尝试计）」）。
if limit > 0 and current_count >= limit then
  return {0, current_count, 0, 0}  -- {allowed=false, current_count, tracked=0, referenced=0}
end

-- 4. Track this attempt (member 唯一，重复登记只会刷新时间戳).
redis.call('ZADD', provider_key, now, member)

-- 5. Reference count: 每次尝试自带一个引用（与成员一一对应），释放一次即摘除。
-- 保留这张 HASH 是为了让释放路径仍能判「重复释放」（refs 已为 0 时不再 ZREM）。
redis.call('HSET', ref_key, member, 1)

-- 6. Set TTL (at least 1h to cover active attempts)
local ttl_seconds = math.floor(ttl / 1000)
local expire_ttl = math.max(3600, ttl_seconds)
redis.call('EXPIRE', provider_key, expire_ttl)
redis.call('EXPIRE', ref_key, expire_ttl)

-- 7. Return success
return {1, current_count + 1, 1, 1}  -- {allowed=true, new_count, tracked=1, referenced=1}
