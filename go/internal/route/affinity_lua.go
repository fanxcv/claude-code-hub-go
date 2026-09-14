package route

// 亲和 Lua 脚本的 Go 侧逐字节副本。
//
// 真源是 src/app/v1/_lib/proxy/affinity/affinity-store.ts 里的模板字面量（不是 lua/ 目录：
// 亲和脚本从未进入 Node 的脚本清单，故无法走 internal/ratelimit 的 EvalConst 注册表）。
// 本文件是副本，不是译者：任何「顺手优化」都会让两侧的 generation fence 语义分叉。
// 漂移由 affinity_lua_test.go 的逐字节比对测试兜住——它直接读 Node 真源并比对，
// 因此 Node 侧改动后本包测试会报红。

// affinityLookupCandidatesLua 对应 Node 的 LOOKUP_CANDIDATES_LUA。
const affinityLookupCandidatesLua = `
-- affinity_lookup_candidates_v4
local legacyGeneration = redis.call('GET', KEYS[#KEYS]) or '0'
local candidates = {}

for i = 1, #KEYS - 1 do
  local v = redis.call('GET', KEYS[i])
  if v and string.sub(v, 1, 2) == '1|' then
    local fourPartProvider = string.match(v, '^1|([^|]+)|([^|]+)|([^|]+)$')
    local threePartProvider, threePartGeneration = string.match(v, '^1|([^|]+)|([^|]+)$')
    local twoPartProvider = string.match(v, '^1|([^|]+)$')
    if fourPartProvider or twoPartProvider or
       (threePartProvider and threePartGeneration == legacyGeneration) then
      candidates[#candidates + 1] = i
      candidates[#candidates + 1] = v
    end
  end
end
return candidates
`

// affinityValidateHitLua 对应 Node 的 VALIDATE_LOOKUP_HIT_LUA。
const affinityValidateHitLua = `
-- affinity_validate_hit_v5
local expectedValue = ARGV[1]
local expectedGeneration = ARGV[2]
local migratedValue = ARGV[3]
local ttl = tonumber(ARGV[4])
local generationTtl = tonumber(ARGV[5])
local now = tonumber(ARGV[6])
local bindingExpiresAt = tonumber(ARGV[7])

local function extendTtl(key, requestedTtl)
  if not requestedTtl or requestedTtl <= 0 then return end
  local currentTtl = redis.call('TTL', key)
  if currentTtl < requestedTtl then
    redis.call('EXPIRE', key, requestedTtl)
  end
end

if redis.call('GET', KEYS[1]) ~= expectedValue then
  return 0
end
redis.call('SET', KEYS[2], expectedGeneration, 'EX', generationTtl, 'NX')
if redis.call('GET', KEYS[2]) ~= expectedGeneration then
  return 0
end
if migratedValue ~= '' then
  redis.call('SET', KEYS[1], migratedValue, 'KEEPTTL')
end
if ttl and ttl > 0 then
  redis.call('EXPIRE', KEYS[1], ttl)
end
extendTtl(KEYS[2], generationTtl)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)
if ttl and ttl > 0 then
  redis.call('ZADD', KEYS[3], bindingExpiresAt, KEYS[1])
  extendTtl(KEYS[3], ttl)
end
return 1
`

// affinityEnsureGenerationLua 对应 Node 的 ENSURE_GENERATION_LUA。
const affinityEnsureGenerationLua = `
-- affinity_ensure_generation_v4
local candidateGeneration = ARGV[1]
local generationTtl = tonumber(ARGV[2])
redis.call('SET', KEYS[1], candidateGeneration, 'EX', generationTtl, 'NX')
local currentTtl = redis.call('TTL', KEYS[1])
if currentTtl < generationTtl then
  redis.call('EXPIRE', KEYS[1], generationTtl)
end
return redis.call('GET', KEYS[1])
`

// affinityCASWriteLua 对应 Node 的 CAS_WRITE_LUA。
const affinityCASWriteLua = `
-- affinity_cas_write_v3
local generation = redis.call('GET', KEYS[1])
if not generation or generation ~= ARGV[1] then
  return 0
end
redis.call('SET', KEYS[2], ARGV[2], 'EX', tonumber(ARGV[3]))
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', tonumber(ARGV[5]))
redis.call('ZADD', KEYS[3], tonumber(ARGV[6]), KEYS[2])
local registryTtl = redis.call('TTL', KEYS[3])
if registryTtl < tonumber(ARGV[3]) then
  redis.call('EXPIRE', KEYS[3], tonumber(ARGV[3]))
end
local generationTtl = redis.call('TTL', KEYS[1])
if generationTtl < tonumber(ARGV[4]) then
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[4]))
end
return 1
`

// affinityInvalidateLua 对应 Node 的 INVALIDATE_LUA。
const affinityInvalidateLua = `
-- affinity_invalidate_v3
local generation = ARGV[1]
redis.call('SET', KEYS[1], generation, 'EX', tonumber(ARGV[2]))
for i = 4, #KEYS do
  redis.call('DEL', KEYS[i])
end
redis.call('DEL', KEYS[2])
redis.call('DEL', KEYS[3])
return generation
`
