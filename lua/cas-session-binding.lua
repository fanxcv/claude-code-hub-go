
local binding_key = KEYS[1]
local legacy_provider_key = KEYS[2]
local legacy_owner_key = KEYS[3]

local current_key_id = ARGV[1]
local expected_generation = ARGV[2]
local next_generation = ARGV[3]
local next_provider_id = ARGV[4]
local ttl = tonumber(ARGV[5])

local function is_positive_integer(value)
  local parsed = tonumber(value)
  return parsed and parsed > 0 and parsed == math.floor(parsed)
end

if not ttl or ttl <= 0 or current_key_id == '' or expected_generation == '' or
   next_generation == '' or not is_positive_integer(next_provider_id) then
  return {'conflict', 'invalid_input'}
end
if redis.call('EXISTS', binding_key) == 0 then
  return {'conflict', 'canonical_missing'}
end

local binding = redis.call('HMGET', binding_key, 'key_id', 'generation', 'provider_id')
local binding_key_id = binding[1]
local generation = binding[2]
local current_provider_id = binding[3]

if not binding_key_id or not generation or binding_key_id == '' or generation == '' then
  return {'conflict', 'canonical_corrupt'}
end
if binding_key_id ~= current_key_id then
  return {'conflict', 'canonical_key_mismatch'}
end
if generation ~= expected_generation then
  return {'conflict', 'generation_mismatch'}
end

local legacy_owner = redis.call('GET', legacy_owner_key)
local legacy_provider = redis.call('GET', legacy_provider_key)
if current_provider_id and not is_positive_integer(current_provider_id) then
  return {'conflict', 'canonical_corrupt'}
end
if legacy_provider and not is_positive_integer(legacy_provider) then
  return {'conflict', 'invalid_legacy_provider'}
end
if not legacy_owner then
  return {'conflict', 'mirror_missing'}
end
if legacy_owner ~= current_key_id then
  return {'conflict', 'foreign_legacy_owner'}
end
if current_provider_id then
  if legacy_provider ~= current_provider_id then
    return {'conflict', 'mirror_conflict'}
  end
elseif legacy_provider then
  return {'conflict', 'mirror_conflict'}
end

redis.call('HSET', binding_key,
  'key_id', current_key_id,
  'generation', next_generation,
  'provider_id', next_provider_id)
redis.call('EXPIRE', binding_key, ttl)
redis.call('SETEX', legacy_owner_key, ttl, current_key_id)
redis.call('SETEX', legacy_provider_key, ttl, next_provider_id)
return {'ok', 'updated', next_generation, next_provider_id}
