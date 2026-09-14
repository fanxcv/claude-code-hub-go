
local binding_key = KEYS[1]
local legacy_provider_key = KEYS[2]
local legacy_owner_key = KEYS[3]

local current_key_id = ARGV[1]
local new_generation = ARGV[2]
local ttl = tonumber(ARGV[3])

local function is_positive_integer(value)
  local parsed = tonumber(value)
  return parsed and parsed > 0 and parsed == math.floor(parsed)
end

if not ttl or ttl <= 0 or current_key_id == '' or new_generation == '' then
  return {'conflict', 'invalid_input'}
end

local legacy_provider = redis.call('GET', legacy_provider_key)
local legacy_owner = redis.call('GET', legacy_owner_key)
local binding_exists = redis.call('EXISTS', binding_key) == 1

if binding_exists then
  local binding = redis.call('HMGET', binding_key, 'key_id', 'generation', 'provider_id')
  local binding_key_id = binding[1]
  local generation = binding[2]
  local provider_id = binding[3]

  if not binding_key_id or not generation or binding_key_id == '' or generation == '' then
    return {'conflict', 'canonical_corrupt'}
  end
  if binding_key_id ~= current_key_id then
    return {'conflict', 'canonical_key_mismatch'}
  end
  if not legacy_owner then
    return {'conflict', 'mirror_missing'}
  end
  if legacy_owner ~= current_key_id then
    return {'conflict', 'foreign_legacy_owner'}
  end

  if provider_id then
    if not is_positive_integer(provider_id) then
      return {'conflict', 'canonical_corrupt'}
    end
    if legacy_provider ~= provider_id then
      return {'conflict', 'mirror_conflict'}
    end
    redis.call('EXPIRE', legacy_provider_key, ttl)
  elseif legacy_provider then
    return {'conflict', 'mirror_conflict'}
  end

  redis.call('EXPIRE', binding_key, ttl)
  redis.call('EXPIRE', legacy_owner_key, ttl)
  return {'ok', 'existing', generation, provider_id or ''}
end

if legacy_owner and legacy_owner ~= current_key_id then
  return {'conflict', 'foreign_legacy_owner'}
end
if legacy_provider and not legacy_owner then
  return {'conflict', 'orphan_legacy_provider'}
end
if legacy_provider and not is_positive_integer(legacy_provider) then
  return {'conflict', 'invalid_legacy_provider'}
end

if not legacy_owner and not legacy_provider then
  redis.call('HSET', binding_key, 'key_id', current_key_id, 'generation', new_generation)
  redis.call('HDEL', binding_key, 'provider_id')
  redis.call('EXPIRE', binding_key, ttl)
  redis.call('SETEX', legacy_owner_key, ttl, current_key_id)
  return {'ok', 'created', new_generation, ''}
end

-- At this point the legacy owner is current_key_id. The provider may be absent,
-- which is the valid null-binding mirror used by a fresh session.
redis.call('HSET', binding_key, 'key_id', current_key_id, 'generation', new_generation)
if legacy_provider then
  redis.call('HSET', binding_key, 'provider_id', legacy_provider)
  redis.call('EXPIRE', legacy_provider_key, ttl)
else
  redis.call('HDEL', binding_key, 'provider_id')
end
redis.call('EXPIRE', binding_key, ttl)
redis.call('EXPIRE', legacy_owner_key, ttl)
return {'ok', 'legacy_upgraded', new_generation, legacy_provider or ''}
