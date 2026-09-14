
local lease_key = KEYS[1]
local owner_token = ARGV[1]
local ttl = tonumber(ARGV[2])

if owner_token == '' or not ttl or ttl <= 0 then
  return 0
end
if redis.call('GET', lease_key) ~= owner_token then
  return 0
end

return redis.call('EXPIRE', lease_key, ttl)
