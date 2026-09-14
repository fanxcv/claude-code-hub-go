
if redis.call('EXISTS', KEYS[1]) == 0 then
  redis.call('SETEX', KEYS[1], ARGV[2], ARGV[1])
  return 1
end
return 0
