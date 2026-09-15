package limit

// 本文件承载租约的两段 Lua 正文，**逐字**取自 Node
// `src/lib/rate-limit/lease-service.ts`（DECREMENT_LUA_SCRIPT 与
// SETTLE_LEASE_BUDGETS_LUA_SCRIPT）。
//
// 为什么不放进 `lua/`：仓库根的 `lua/*.lua` 是 `src/lib/redis/lua-scripts.ts` 的
// 语言中立导出（由 scripts/verify-lua-parity.ts 逐字节守护），而这两段脚本在 Node 侧是
// 写在 lease-service.ts 里的**内联**常量，不属于那个注册表。放错地方会让 parity 校验
// 报「磁盘多出脚本」。因此这里以原文常量内嵌，并靠单元测试钉住语义（见 lease_test.go）。
//
// 两段脚本都必须原样保留的关键点：
//   - 判定与改写在同一段 Lua 内完成（Redis 单线程执行即原子），Go 侧不得拆成多条命令；
//   - 不足时把剩余额度**清零**并写回（Node 注释：否则刷新窗口内的每个请求都会重复同一笔超支）；
//   - 结算脚本用 KEYS[1] 做幂等标记，重放时直接返回上一次的结果。

// decrementLeaseBudgetLua 原子扣减单份租约。
//
// KEYS[1]=租约键，ARGV[1]=要扣的成本；返回 {newRemaining, success}：
// success=1 已扣减；success=0 且 newRemaining=0 表示额度不足；newRemaining=-1 表示键不存在。
const decrementLeaseBudgetLua = `
    local key = KEYS[1]
    local cost = tonumber(ARGV[1])

    -- Get current lease JSON
    local leaseJson = redis.call('GET', key)
    if not leaseJson then
      return {-1, 0}
    end

    -- Parse lease JSON
    local lease = cjson.decode(leaseJson)
    local remaining = tonumber(lease.remainingBudget) or 0

    -- Check if budget is sufficient
    if remaining < cost then
      return {0, 0}
    end

    -- Decrement budget
    local newRemaining = remaining - cost
    lease.remainingBudget = newRemaining

    -- Get TTL and update lease
    local ttl = redis.call('TTL', key)
    if ttl > 0 then
      redis.call('SETEX', key, ttl, cjson.encode(lease))
    end

    return {newRemaining, 1}
  `

// settleLeaseBudgetsLua 原子结算 4 窗口 × 3 主体共 12 份租约。
//
// KEYS[1]=幂等标记键，KEYS[2..13]=按 key/user/provider 再 5h/daily/weekly/monthly 排序的租约键；
// ARGV[1]=本次实际成本，ARGV[2]=标记 TTL 秒。返回 {是否命中旧标记, 结算结果 JSON 字符串}。
const settleLeaseBudgetsLua = `
    local markerKey = KEYS[1]
    local previousSettlement = redis.call("GET", markerKey)
    if previousSettlement then
      return {1, previousSettlement}
    end

    local cost = tonumber(ARGV[1])
    local markerTtlSeconds = tonumber(ARGV[2])
    local settlements = {}
    local pendingWrites = {}

    for keyIndex = 2, #KEYS do
      local leaseKey = KEYS[keyIndex]
      local leaseReply = redis.pcall("GET", leaseKey)
      local leaseReadFailed = type(leaseReply) == "table" and leaseReply.err

      if leaseReadFailed or not leaseReply then
        settlements[#settlements + 1] = {0, -1}
      else
        local decoded, lease = pcall(cjson.decode, leaseReply)
        local remaining = nil
        if decoded and type(lease) == "table" then
          remaining = tonumber(lease.remainingBudget)
        end
        local ttl = redis.call("TTL", leaseKey)

        if not remaining or ttl <= 0 then
          settlements[#settlements + 1] = {0, -1}
        elseif remaining < cost then
          -- Consume the cached slice when the request is larger than the
          -- remaining lease. Keeping a positive balance here lets every
          -- request in the refresh window repeat the same overshoot.
          lease.remainingBudget = 0
          local encodedLeaseOk, encodedLease = pcall(cjson.encode, lease)
          if not encodedLeaseOk then
            settlements[#settlements + 1] = {0, -1}
          else
            pendingWrites[#pendingWrites + 1] = {leaseKey, ttl, encodedLease}
            settlements[#settlements + 1] = {-1, 0}
          end
        else
          local newRemaining = remaining - cost
          lease.remainingBudget = newRemaining
          local encodedLeaseOk, encodedLease = pcall(cjson.encode, lease)
          if not encodedLeaseOk then
            settlements[#settlements + 1] = {0, -1}
          else
            pendingWrites[#pendingWrites + 1] = {leaseKey, ttl, encodedLease}
            settlements[#settlements + 1] = {1, newRemaining}
          end
        end
      end
    end

    local encodedOk, encoded = pcall(cjson.encode, settlements)
    if not encodedOk then
      return redis.error_reply("failed to encode lease settlement results")
    end

    for writeIndex = 1, #pendingWrites do
      local pendingWrite = pendingWrites[writeIndex]
      redis.call("SETEX", pendingWrite[1], pendingWrite[2], pendingWrite[3])
    end

    redis.call("SETEX", markerKey, markerTtlSeconds, encoded)
    return {0, encoded}
  `
