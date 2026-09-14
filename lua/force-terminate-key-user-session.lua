
local session_id = ARGV[1]
local removed_global = redis.call('ZREM', KEYS[1], session_id)
local removed_key = redis.call('ZREM', KEYS[2], session_id)
local removed_user = redis.call('ZREM', KEYS[3], session_id)
return {removed_global, removed_key, removed_user}
