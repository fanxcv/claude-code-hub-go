
local provider_key = KEYS[1]
local ref_key = KEYS[2]
local session_id = ARGV[1]
local limit = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4]) or 300000

-- Guard against invalid TTL (prevents clearing all sessions)
if ttl <= 0 then
  ttl = 300000
end

-- 1. Cleanup expired sessions (TTL window ago)
local cutoff = now - ttl
local expired_sessions = redis.call('ZRANGEBYSCORE', provider_key, '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', provider_key, '-inf', cutoff)
for _, expired_session_id in ipairs(expired_sessions) do
  redis.call('HDEL', ref_key, expired_session_id)
end

-- 2. Check if session is already tracked
local is_tracked = redis.call('ZSCORE', provider_key, session_id)

-- Direct cleanup paths may remove the ZSET member before this script sees the session again.
-- When the member is absent, discard any stale reference hash value before acquiring a new ref.
if not is_tracked then
  redis.call('HDEL', ref_key, session_id)
end

local existing_refs = tonumber(redis.call('HGET', ref_key, session_id) or '0')

-- 3. Get current concurrency count
local current_count = redis.call('ZCARD', provider_key)

-- 4. Check limit (exclude already tracked session)
if limit > 0 and not is_tracked and current_count >= limit then
  return {0, current_count, 0, 0}  -- {allowed=false, current_count, tracked=0, referenced=0}
end

-- 5. Track session (ZADD updates timestamp for existing members)
redis.call('ZADD', provider_key, now, session_id)

local referenced = 0
if not is_tracked or existing_refs > 0 then
  redis.call('HINCRBY', ref_key, session_id, 1)
  referenced = 1
end

-- 6. Set TTL based on session TTL (at least 1h to cover active sessions)
local ttl_seconds = math.floor(ttl / 1000)
local expire_ttl = math.max(3600, ttl_seconds)
redis.call('EXPIRE', provider_key, expire_ttl)
redis.call('EXPIRE', ref_key, expire_ttl)

-- 7. Return success
if is_tracked then
  -- Already tracked, count unchanged
  return {1, current_count, 0, referenced}  -- {allowed=true, count, tracked=0, referenced}
else
  -- New tracking, count +1
  return {1, current_count + 1, 1, referenced}  -- {allowed=true, new_count, tracked=1, referenced=1}
end
