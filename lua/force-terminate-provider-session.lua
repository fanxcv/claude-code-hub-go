
local provider_key = KEYS[1]
local ref_key = KEYS[2]
local session_id = ARGV[1]

local removed_refs = redis.call('HDEL', ref_key, session_id)
local removed_session = redis.call('ZREM', provider_key, session_id)
return {removed_session, removed_refs}
