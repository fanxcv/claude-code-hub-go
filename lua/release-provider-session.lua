
local provider_key = KEYS[1]
local ref_key = KEYS[2]
local session_id = ARGV[1]

local current_refs = tonumber(redis.call('HGET', ref_key, session_id) or '0')
if current_refs <= 0 then
  return {0, 0}
end

local remaining_refs = current_refs - 1
if remaining_refs > 0 then
  redis.call('HSET', ref_key, session_id, remaining_refs)
  return {0, remaining_refs}
end

redis.call('HDEL', ref_key, session_id)
local removed = redis.call('ZREM', provider_key, session_id)
return {removed, remaining_refs}
