
local global_key = KEYS[1]
local key_key = KEYS[2]
local user_key = KEYS[3]

local session_id = ARGV[1]
local key_limit = tonumber(ARGV[2])
local user_limit = tonumber(ARGV[3])
local now = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5]) or 300000

-- Guard against invalid TTL (prevents clearing all sessions)
if ttl <= 0 then
  ttl = 300000
end

-- 1. Cleanup expired sessions (TTL window ago)
local cutoff = now - ttl
redis.call('ZREMRANGEBYSCORE', global_key, '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', key_key, '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', user_key, '-inf', cutoff)

-- 2. Check if session is already tracked
local is_tracked_key = redis.call('ZSCORE', key_key, session_id)
local is_tracked_user = redis.call('ZSCORE', user_key, session_id)

-- 3. Get current concurrency counts
local current_key_count = redis.call('ZCARD', key_key)
local current_user_count = redis.call('ZCARD', user_key)

-- 4. Check Key limit (exclude already tracked session)
if key_limit > 0 and not is_tracked_key and current_key_count >= key_limit then
  return {0, 1, current_key_count, 0, current_user_count, 0}
end

-- 5. Check User limit (exclude already tracked session)
-- 说明：User 上限以 user ZSET 为准；key ZSET 不参与 user 维度的“已追踪”判定，避免绕过 user 并发限制。
if user_limit > 0 and not is_tracked_user and current_user_count >= user_limit then
  return {0, 2, current_key_count, 0, current_user_count, 0}
end

-- 6. Track session (ZADD updates timestamp for existing members)
redis.call('ZADD', global_key, now, session_id)
redis.call('ZADD', key_key, now, session_id)
redis.call('ZADD', user_key, now, session_id)

-- 7. Set TTL based on session TTL (at least 1h to cover active sessions)
local ttl_seconds = math.floor(ttl / 1000)
local expire_ttl = math.max(3600, ttl_seconds)
redis.call('EXPIRE', global_key, expire_ttl)
redis.call('EXPIRE', key_key, expire_ttl)
redis.call('EXPIRE', user_key, expire_ttl)

-- 8. Return success (compute counts)
local key_count = current_key_count
local key_tracked = 0
if not is_tracked_key then
  key_count = key_count + 1
  key_tracked = 1
end

local user_count = current_user_count
local user_tracked = 0
if not is_tracked_user then
  user_count = user_count + 1
  user_tracked = 1
end

return {1, 0, key_count, key_tracked, user_count, user_tracked}
