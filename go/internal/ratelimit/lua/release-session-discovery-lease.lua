
local lease_key = KEYS[1]
local owner_token = ARGV[1]

if owner_token == '' or redis.call('GET', lease_key) ~= owner_token then
  return 0
end

return redis.call('DEL', lease_key)
