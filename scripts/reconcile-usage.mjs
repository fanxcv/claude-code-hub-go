// 用量/成本对账（只读）：按时间窗重算成本与用量，与库内成本字段比对，差异即告警。
//
// 用法：
//   CCH_DSN=postgres://… node scripts/reconcile-usage.mjs                    # 默认最近 24 小时
//   CCH_DSN=… node scripts/reconcile-usage.mjs --window 7d                   # 最近 7 天
//   CCH_DSN=… node scripts/reconcile-usage.mjs --from 2026-09-01T00:00:00+08:00 \
//     --to 2026-09-02T00:00:00+08:00
//   node scripts/reconcile-usage.mjs --self-test                             # 不连库，验解析与判定
//   node scripts/reconcile-usage.mjs --dry-run                               # 只打印计划与窗口
//   node scripts/reconcile-usage.mjs --strict-price                          # 把价目重算差异也算失败
//
// 判据（三项，前两项为准入硬检查；第三项默认只报不判）：
//   1. 账本一致性：窗口内应计账的 message_request 行，usage_ledger 必须存在且 cost_usd 相等。
//      「应计账」的口径直接取自触发器 fn_upsert_usage_ledger（drizzle/0116_gigantic_zombie.sql）：
//        - blocked_by = 'warmup' 的行不建账本行；
//        - endpoint 归一化后为 /v1/messages/count_tokens 或 /v1/responses/compact 的行删账本行；
//        - is_replay 的行账本 cost_usd 记 0（不是 0 就是缺陷）。
//      软删行不排除：deleted_at 不在触发器监视列里，账本行按设计保留。
//   2. breakdown 内部自洽：cost_breakdown 存在时
//        total == base_total × provider_multiplier × group_multiplier
//        base_total == input + output + cache_creation + cache_read
//        cache_creation == cache_creation_5m + cache_creation_1h（两者都在时）
//        cost_usd == total，cost_multiplier/group_cost_multiplier 与 breakdown 同值（按列精度比）
//      依据 cost-calculation.ts:245 与 response-handler.ts:6771 的不变量注释。
//   3. 价目重算（独立复算）：用 model_prices 的最新一代价目 × token 数 × 行内倍率复算，与 cost_usd 比对。
//      已镜像 Node 的：供应商档选择（三级）、分层价（200k/272k 阈值与「有真实基价才启用分层」的护栏）、
//      缓存 5m/1h 拆分、缓存读与缓存创建的回退单价。抽样按模型覆盖（每模型 ≤25 行）而非只取最贵行。
//      **priority 档**：行里不持久化「本次是否走 priority 计费」，故两种模式各算一遍，
//      任一命中即通过（报告里 matchedPriorityOnly 会显示只被 priority 模式解释的行数）。
//      仍不可比的行按原因登记（**不判失败**）：无价目行、hedge 输家（费用已并入 cost_usd）、
//      input_cost_per_request（按次计费）、long_context_pricing（显式分层未持久化）、图片 token、未知缓存 TTL。
//      需要它判失败时用 --strict-price（只对「可比」行判）。
//
// 退出码：0 无差异；1 有差异（含 --strict-price 下的重算差异）；2 配置/依赖错误。
//
// 硬约束：只读（不加锁、不写库、不改 DDL）；凭据只走环境变量，绝不出现在 argv 与输出里。
//
// ponytail: 复算的单价选择只做「供应商键精确命中 → official 档 → 顶层」三级，
// 不实现 Node 的 provider×model×url 全量候选解析；需要逐字对价时把 pricing-resolution.ts 搬过来。

import { execFileSync } from "node:child_process";
import process from "node:process";

const COST_SCALE = 15;

class ConfigError extends Error {}

// ---------------------------------------------------------------------------
// 参数与窗口
// ---------------------------------------------------------------------------
function readConfig(argv) {
  const flags = new Set(argv.filter((a) => a.startsWith("--")));
  const known = [
    "--dry-run",
    "--json",
    "--help",
    "--self-test",
    "--strict-price",
    "--window",
    "--from",
    "--to",
    "--price-sample",
  ];
  for (const arg of argv) {
    if (arg.startsWith("--") && !known.includes(arg)) throw new ConfigError(`未知参数 ${arg}`);
  }
  const valueOf = (name) => {
    const idx = argv.indexOf(name);
    if (idx < 0) return "";
    const value = argv[idx + 1];
    if (!value || value.startsWith("--")) throw new ConfigError(`${name} 需要一个取值`);
    return value.trim();
  };
  const num = (name, fallback, min, max) => {
    const raw = valueOf(name);
    if (!raw) return fallback;
    const parsed = Number(raw);
    if (!Number.isFinite(parsed) || parsed < min || parsed > max) {
      throw new ConfigError(`${name} 需为 ${min}..${max} 的数，收到 ${raw}`);
    }
    return Math.floor(parsed);
  };

  const env = process.env;
  const config = {
    dryRun: flags.has("--dry-run"),
    json: flags.has("--json"),
    help: flags.has("--help"),
    selfTest: flags.has("--self-test"),
    strictPrice: flags.has("--strict-price"),
    window: valueOf("--window") || "24h",
    from: valueOf("--from"),
    to: valueOf("--to"),
    priceSample: num("--price-sample", 200, 1, 5000),
    dsn: env.CCH_DSN?.trim() || env.DSN?.trim() || "",
  };
  if (config.dryRun || config.selfTest || config.help) return config;
  if (!config.dsn) throw new ConfigError("需要 CCH_DSN（或 DSN）：对账要连库只读");
  return config;
}

const WINDOW_UNIT_MS = { m: 60_000, h: 3_600_000, d: 86_400_000 };

// resolveWindow 把 --window/--from/--to 归一为半开区间 [from, to)。
// 相对窗口用「现在」为右端点；闭区间写法（--from/--to）右端点按用户给的值原样使用。
function resolveWindow(config, now = new Date()) {
  if (config.from || config.to) {
    if (!config.from || !config.to) throw new ConfigError("--from 与 --to 必须成对给出");
    const from = parseInstant(config.from, "--from");
    const to = parseInstant(config.to, "--to");
    if (to.getTime() <= from.getTime()) throw new ConfigError("--to 必须晚于 --from");
    return { from, to, label: `${config.from} .. ${config.to}` };
  }
  const match = /^(\d+)([mhd])$/.exec(config.window);
  if (!match) throw new ConfigError(`--window 需形如 30m/24h/7d，收到 ${config.window}`);
  const spanMs = Number(match[1]) * WINDOW_UNIT_MS[match[2]];
  if (spanMs <= 0) throw new ConfigError("--window 必须为正");
  return {
    from: new Date(now.getTime() - spanMs),
    to: now,
    label: `最近 ${config.window}`,
  };
}

function parseInstant(raw, flag) {
  const parsed = new Date(raw);
  if (Number.isNaN(parsed.getTime())) throw new ConfigError(`${flag} 不是可解析的时间：${raw}`);
  return parsed;
}

function isoSeconds(date) {
  return date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

// ---------------------------------------------------------------------------
// 数据库：只经 psql 子进程，凭据只走环境变量
// ---------------------------------------------------------------------------
function parseDsn(dsn) {
  const url = new URL(dsn);
  if (url.protocol !== "postgres:" && url.protocol !== "postgresql:") {
    throw new ConfigError(`DSN 协议应为 postgres://，收到 ${url.protocol}`);
  }
  return {
    host: url.hostname,
    port: url.port || "5432",
    user: decodeURIComponent(url.username),
    password: decodeURIComponent(url.password),
    database: url.pathname.replace(/^\//, ""),
  };
}

// psqlJson 跑一条**只读**查询并要求它返回单个 JSON 值。
function psqlJson(db, sql) {
  const out = execFileSync(
    "psql",
    ["-At", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=terse", "-c", sql],
    {
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        PGHOST: db.host,
        PGPORT: db.port,
        PGUSER: db.user,
        PGPASSWORD: db.password,
        PGDATABASE: db.database,
        PGOPTIONS: "-c default_transaction_read_only=on",
      },
    }
  ).trim();
  if (!out) throw new ConfigError("psql 没有返回结果");
  return JSON.parse(out);
}

// ---------------------------------------------------------------------------
// 三项检查的 SQL
//
// 时间窗与排除口径写在 CTE 里，三项检查共用同一份 scope 语义（保证它们看的是同一批行）。
// 所有数值比较都在 PG 的 numeric 上做：JSON 往返会把 numeric 变成 float 而丢精度。
// ---------------------------------------------------------------------------
const SCOPE_CTE = (fromIso, toIso, extraFilter = "") => `
  WITH win AS (SELECT '${fromIso}'::timestamptz AS lo, '${toIso}'::timestamptz AS hi),
  scope AS (
    SELECT m.*
    FROM message_request m, win w
    WHERE m.created_at >= w.lo AND m.created_at < w.hi
      -- 与触发器 fn_upsert_usage_ledger 同一套「不建账本行」口径
      AND coalesce(m.blocked_by, '') <> 'warmup'
      AND lower(regexp_replace(coalesce(m.endpoint, ''), '/+$', ''))
          NOT IN ('/v1/messages/count_tokens', '/v1/responses/compact')
      ${extraFilter}
  )`;

// 检查 1：账本一致性。
function ledgerSql(fromIso, toIso) {
  return `
${SCOPE_CTE(fromIso, toIso)}
, joined AS (
  SELECT s.id, s.is_replay, s.cost_usd AS req_cost, l.cost_usd AS led_cost,
         (l.request_id IS NULL) AS missing,
         (l.request_id IS NOT NULL
          AND l.cost_usd IS DISTINCT FROM (CASE WHEN s.is_replay THEN 0::numeric ELSE s.cost_usd END))
           AS divergent
  FROM scope s
  LEFT JOIN usage_ledger l ON l.request_id = s.id
)
SELECT json_build_object(
  'rows', count(*),
  'missing', count(*) FILTER (WHERE missing),
  'divergent', count(*) FILTER (WHERE divergent),
  'requestTotal', coalesce(sum(CASE WHEN is_replay THEN 0::numeric ELSE coalesce(req_cost, 0) END), 0)::text,
  'ledgerTotal', coalesce(sum(coalesce(led_cost, 0)), 0)::text,
  'samples', coalesce(
    (SELECT json_agg(x) FROM (
      SELECT id, missing, divergent, req_cost::text AS request_cost_usd, led_cost::text AS ledger_cost_usd
      FROM joined WHERE missing OR divergent ORDER BY id LIMIT 20
    ) x), '[]'::json)
) FROM joined`;
}

// 检查 2：cost_breakdown 内部自洽。
function breakdownSql(fromIso, toIso) {
  return `
${SCOPE_CTE(fromIso, toIso, "AND m.cost_breakdown IS NOT NULL")}
, calc AS (
  SELECT s.id,
         s.cost_usd::text AS cost_usd,
         (s.cost_breakdown->>'total')::numeric AS total,
         (s.cost_breakdown->>'base_total')::numeric AS base_total,
         (s.cost_breakdown->>'input')::numeric AS c_input,
         (s.cost_breakdown->>'output')::numeric AS c_output,
         (s.cost_breakdown->>'cache_creation')::numeric AS c_cache_creation,
         (s.cost_breakdown->>'cache_creation_5m')::numeric AS c_cache_creation_5m,
         (s.cost_breakdown->>'cache_creation_1h')::numeric AS c_cache_creation_1h,
         (s.cost_breakdown->>'cache_read')::numeric AS c_cache_read,
         (s.cost_breakdown->>'provider_multiplier')::numeric AS cb_pm,
         (s.cost_breakdown->>'group_multiplier')::numeric AS cb_gm,
         s.cost_multiplier AS col_pm,
         s.group_cost_multiplier AS col_gm
  FROM scope s
)
, checks AS (
  SELECT id, cost_usd, total, base_total, cb_pm, cb_gm,
         (total <> base_total * cb_pm * cb_gm) AS total_mismatch,
         (base_total <> c_input + c_output + c_cache_creation + c_cache_read) AS base_mismatch,
         (c_cache_creation_5m IS NOT NULL AND c_cache_creation_1h IS NOT NULL
            AND c_cache_creation <> c_cache_creation_5m + c_cache_creation_1h) AS cache_split_mismatch,
         (total <> cost_usd::numeric) AS cost_mismatch,
         -- 倍率列是 numeric(10,4)：按列自身精度比，避免把存储舍入误判成缺陷
         (col_pm IS NOT NULL AND cb_pm IS NOT NULL AND col_pm <> round(cb_pm, 4)) AS pm_mismatch,
         (col_gm IS NOT NULL AND cb_gm IS NOT NULL AND col_gm <> round(cb_gm, 4)) AS gm_mismatch
  FROM calc
)
SELECT json_build_object(
  'rows', count(*),
  'totalMismatch', count(*) FILTER (WHERE total_mismatch),
  'baseMismatch', count(*) FILTER (WHERE base_mismatch),
  'cacheSplitMismatch', count(*) FILTER (WHERE cache_split_mismatch),
  'costMismatch', count(*) FILTER (WHERE cost_mismatch),
  'multiplierMismatch', count(*) FILTER (WHERE pm_mismatch OR gm_mismatch),
  'samples', coalesce(
    (SELECT json_agg(x) FROM (
      SELECT id, total::text AS total, base_total::text AS base_total,
             cb_pm::text AS provider_multiplier, cb_gm::text AS group_multiplier,
             cost_usd, total_mismatch, base_mismatch, cache_split_mismatch, cost_mismatch,
             pm_mismatch, gm_mismatch
      FROM checks
      WHERE total_mismatch OR base_mismatch OR cache_split_mismatch OR cost_mismatch
         OR pm_mismatch OR gm_mismatch
      ORDER BY id LIMIT 20
    ) x), '[]'::json)
) FROM checks`;
}

// 检查 3：用 model_prices 独立复算。
//
// 价目行解析镜像 src/repository/model-price.ts 的 findLatestPriceByModel：
// 先精确名（manual 优先、created_at 倒序、id 倒序），未命中再走别名候选（aliases ? 原名）。
// 单价档位只做三级：供应商键精确命中 → 该档 official: true → 顶层字段。
function priceSql(fromIso, toIso, sampleLimit) {
  return `
${SCOPE_CTE(fromIso, toIso, "AND m.status_code IS NOT NULL AND m.is_replay = false AND coalesce(m.cost_usd, 0) <> 0")}
, picked AS (
  SELECT s.id, s.model, s.original_model, s.actual_response_model,
         s.input_tokens, s.output_tokens,
         s.cache_creation_input_tokens, s.cache_creation_5m_input_tokens,
         s.cache_creation_1h_input_tokens, s.cache_read_input_tokens,
         s.cache_ttl_applied, s.cost_usd, s.cost_multiplier, s.group_cost_multiplier,
         coalesce(s.actual_response_model, s.model, s.original_model) AS price_model,
         s.provider_id,
         -- hedge 输家费用已并入 cost_usd，无法用单一价目复算
         (s.hedge_losers IS NOT NULL AND jsonb_typeof(s.hedge_losers) = 'array'
            AND jsonb_array_length(s.hedge_losers) > 0) AS has_hedge_losers
  FROM (
    -- 抽样按模型覆盖：每个模型最多 25 行，再按成本取前 N 行。
    -- 只按成本降序会退化成「一个模型的贵行」——抽样要覆盖多个模型才有对账价值。
    SELECT s.*,
           row_number() OVER (
             PARTITION BY coalesce(s.actual_response_model, s.model, s.original_model)
             ORDER BY s.cost_usd DESC NULLS LAST, s.id
           ) AS model_rank
    FROM scope s
  ) s
  WHERE s.model_rank <= 25
  ORDER BY s.cost_usd DESC NULLS LAST, s.id
  LIMIT ${sampleLimit}
)
, priced AS (
  -- 精确名命中：单独命名，避免与回退段的同名列并存（否则 price_data 在 units 里歧义）
  SELECT p.*, pr.price_data AS exact_price_data
  FROM picked p
  LEFT JOIN LATERAL (
    SELECT mp.price_data
    FROM model_prices mp
    WHERE mp.model_name = p.price_model
    ORDER BY (mp.source = 'manual') DESC, mp.created_at DESC NULLS LAST, mp.id DESC
    LIMIT 1
  ) pr ON true
)
, priced_fallback AS (
  SELECT p.*, coalesce(p.exact_price_data, fb.price_data) AS price_data
  FROM priced p
  LEFT JOIN LATERAL (
    SELECT mp.price_data
    FROM model_prices mp
    WHERE p.exact_price_data IS NULL
      AND (mp.price_data -> 'aliases' ? p.price_model)
    ORDER BY (mp.source = 'manual') DESC, mp.created_at DESC NULLS LAST, mp.id DESC
    LIMIT 1
  ) fb ON true
)
, units AS (
  SELECT pf.*,
         -- 供应商档：优先 provider 名归一化后精确命中 pricing 键，其次是该档 official=true
         coalesce(
           pf.price_data -> 'pricing' -> (SELECT k FROM jsonb_object_keys(coalesce(pf.price_data->'pricing','{}'::jsonb)) k
              WHERE k = regexp_replace(lower(coalesce(pv.name, pv.url, '')), '[^a-z0-9]', '', 'g')
              LIMIT 1),
           (SELECT v FROM jsonb_each(coalesce(pf.price_data->'pricing','{}'::jsonb)) e(k, v)
              WHERE (v->>'official')::boolean IS TRUE LIMIT 1),
           '{}'::jsonb
         ) AS tier
  FROM priced_fallback pf
  LEFT JOIN providers pv ON pv.id = pf.provider_id
)
, raw_rates AS (
  SELECT u.*,
         -- 基础档（顶层或供应商档，供应商档优先）
         coalesce((u.tier->>'input_cost_per_token')::numeric,
                  (u.price_data->>'input_cost_per_token')::numeric) AS base_in,
         coalesce((u.tier->>'output_cost_per_token')::numeric,
                  (u.price_data->>'output_cost_per_token')::numeric) AS base_out,
         coalesce((u.tier->>'cache_creation_input_token_cost')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost')::numeric) AS base_cc,
         coalesce((u.tier->>'cache_creation_input_token_cost_above_1hr')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost_above_1hr')::numeric) AS base_cc1h,
         coalesce((u.tier->>'cache_read_input_token_cost')::numeric,
                  (u.price_data->>'cache_read_input_token_cost')::numeric) AS base_read,
         -- 分层档：Node 的 resolvePriorityAwareLongContextRate 在非 priority 档下取 above272k ?? above200k
         coalesce((u.tier->>'input_cost_per_token_above_272k_tokens')::numeric,
                  (u.tier->>'input_cost_per_token_above_200k_tokens')::numeric,
                  (u.price_data->>'input_cost_per_token_above_272k_tokens')::numeric,
                  (u.price_data->>'input_cost_per_token_above_200k_tokens')::numeric) AS above_in,
         coalesce((u.tier->>'output_cost_per_token_above_272k_tokens')::numeric,
                  (u.tier->>'output_cost_per_token_above_200k_tokens')::numeric,
                  (u.price_data->>'output_cost_per_token_above_272k_tokens')::numeric,
                  (u.price_data->>'output_cost_per_token_above_200k_tokens')::numeric) AS above_out,
         coalesce((u.tier->>'cache_creation_input_token_cost_above_272k_tokens')::numeric,
                  (u.tier->>'cache_creation_input_token_cost_above_200k_tokens')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost_above_272k_tokens')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost_above_200k_tokens')::numeric) AS above_cc,
         coalesce((u.tier->>'cache_creation_input_token_cost_above_1hr_above_272k_tokens')::numeric,
                  (u.tier->>'cache_creation_input_token_cost_above_1hr_above_200k_tokens')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost_above_1hr_above_272k_tokens')::numeric,
                  (u.price_data->>'cache_creation_input_token_cost_above_1hr_above_200k_tokens')::numeric) AS above_cc1h,
         coalesce((u.tier->>'cache_read_input_token_cost_above_272k_tokens')::numeric,
                  (u.tier->>'cache_read_input_token_cost_above_200k_tokens')::numeric,
                  (u.price_data->>'cache_read_input_token_cost_above_272k_tokens')::numeric,
                  (u.price_data->>'cache_read_input_token_cost_above_200k_tokens')::numeric) AS above_read,
         -- 触发量：Node 的 getLongContextTriggerInputTokens（input + 缓存创建 + 缓存读；图片 token 未持久化，见文件头口径上限）
         (coalesce(u.input_tokens, 0)
          + coalesce(u.cache_creation_input_tokens,
                     coalesce(u.cache_creation_5m_input_tokens, 0) + coalesce(u.cache_creation_1h_input_tokens, 0))
          + coalesce(u.cache_read_input_tokens, 0)) AS trigger_tokens,
         -- 阈值：有 272k 档字段或 gpt 家族则 272000，否则 200000（cost-calculation.ts:114-130）
         CASE WHEN EXISTS (
                SELECT 1 FROM jsonb_object_keys(u.price_data) f WHERE f LIKE '%_above_272k_tokens%')
              OR coalesce(u.price_data->>'model_family', '') IN ('gpt', 'gpt-pro')
           THEN 272000 ELSE 200000 END AS threshold_tokens
  FROM units u
)
, rates AS (
  SELECT r.*,
         CASE WHEN r.trigger_tokens > r.threshold_tokens AND r.above_in IS NOT NULL
              THEN r.above_in ELSE r.base_in END AS in_rate,
         CASE WHEN r.trigger_tokens > r.threshold_tokens AND r.above_out IS NOT NULL
              THEN r.above_out ELSE r.base_out END AS out_rate,
         -- 缓存创建 5m：分层只在「有真实基价」时启用（hasRealCacheCreationBase）
         CASE WHEN r.trigger_tokens > r.threshold_tokens AND r.base_cc IS NOT NULL AND r.above_cc IS NOT NULL
              THEN r.above_cc ELSE coalesce(r.base_cc, r.base_in * 1.25) END AS write5m_rate,
         CASE WHEN r.trigger_tokens > r.threshold_tokens AND r.base_cc IS NOT NULL
                   AND coalesce(r.above_cc1h, r.above_cc) IS NOT NULL
              THEN coalesce(r.above_cc1h, r.above_cc)
              ELSE coalesce(r.base_cc1h, r.base_in * 2, r.base_cc, r.base_in * 1.25) END AS write1h_rate,
         -- 缓存读：分层同样要求有真实基价（hasRealCacheReadBase）
         CASE WHEN r.trigger_tokens > r.threshold_tokens AND r.base_read IS NOT NULL AND r.above_read IS NOT NULL
              THEN r.above_read
              ELSE coalesce(r.base_read, r.base_in * 0.1, r.base_out * 0.1) END AS read_rate,
         -- priority 档（cost-calculation.ts 的 priorityServiceTierApplied=true 分支）：
         -- 行里不持久化「本次是否走了 priority 计费」，故两种模式各算一遍，任一命中即通过。
         coalesce((r.tier->>'input_cost_per_token_priority')::numeric,
                  (r.price_data->>'input_cost_per_token_priority')::numeric) AS prio_base_in,
         coalesce((r.tier->>'output_cost_per_token_priority')::numeric,
                  (r.price_data->>'output_cost_per_token_priority')::numeric) AS prio_base_out,
         coalesce((r.tier->>'cache_read_input_token_cost_priority')::numeric,
                  (r.price_data->>'cache_read_input_token_cost_priority')::numeric) AS prio_base_read,
         -- 分层档的 priority 变体优先，否则回落非 priority 分层档（Node 同序）
         coalesce((r.tier->>'input_cost_per_token_above_272k_tokens_priority')::numeric,
                  (r.tier->>'input_cost_per_token_above_200k_tokens_priority')::numeric,
                  (r.price_data->>'input_cost_per_token_above_272k_tokens_priority')::numeric,
                  (r.price_data->>'input_cost_per_token_above_200k_tokens_priority')::numeric,
                  r.above_in) AS prio_above_in,
         coalesce((r.tier->>'output_cost_per_token_above_272k_tokens_priority')::numeric,
                  (r.tier->>'output_cost_per_token_above_200k_tokens_priority')::numeric,
                  (r.price_data->>'output_cost_per_token_above_272k_tokens_priority')::numeric,
                  (r.price_data->>'output_cost_per_token_above_200k_tokens_priority')::numeric,
                  r.above_out) AS prio_above_out,
         coalesce((r.tier->>'cache_read_input_token_cost_above_272k_tokens_priority')::numeric,
                  (r.tier->>'cache_read_input_token_cost_above_200k_tokens_priority')::numeric,
                  (r.price_data->>'cache_read_input_token_cost_above_272k_tokens_priority')::numeric,
                  (r.price_data->>'cache_read_input_token_cost_above_200k_tokens_priority')::numeric,
                  r.above_read) AS prio_above_read,
         (EXISTS (SELECT 1 FROM jsonb_object_keys(r.price_data) f WHERE f LIKE '%_priority')) AS has_priority_fields
  FROM raw_rates r
)
, classified AS (
  SELECT r.*,
    -- 不可比原因（Node 的完整定价语义未在此复刻）：NULL 表示可比
    CASE
      WHEN r.price_data IS NULL THEN 'no_price_row'
      WHEN r.has_hedge_losers THEN 'hedge_losers'
      WHEN r.tier ? 'input_cost_per_request' OR r.price_data ? 'input_cost_per_request'
        THEN 'input_cost_per_request'
      WHEN (r.price_data->>'long_context_pricing') IS NOT NULL THEN 'long_context_pricing'
      WHEN EXISTS (
        SELECT 1 FROM jsonb_object_keys(r.price_data) f WHERE f LIKE 'output_cost_per_image%') THEN 'image_tokens'
      WHEN r.cache_ttl_applied IS NOT NULL AND r.cache_ttl_applied NOT IN ('5m','1h','mixed')
        THEN 'unknown_cache_ttl'
      ELSE NULL
    END AS not_comparable
  FROM rates r
)
, expected AS (
  SELECT c.*,
    -- 5m/1h 拆分：与 cost-calculation.ts:704-716 同口径（聚合值不足时差额记入 5m，1h 档记入 1h）
    greatest(coalesce(c.cache_creation_5m_input_tokens, 0)
      + greatest(coalesce(c.cache_creation_input_tokens, 0)
          - coalesce(c.cache_creation_5m_input_tokens, 0)
          - coalesce(c.cache_creation_1h_input_tokens, 0), 0)
      * CASE WHEN c.cache_ttl_applied = '1h' THEN 0 ELSE 1 END, 0) AS tok_5m,
    (coalesce(c.cache_creation_1h_input_tokens, 0)
      + CASE WHEN c.cache_ttl_applied = '1h'
          THEN greatest(coalesce(c.cache_creation_input_tokens, 0)
            - coalesce(c.cache_creation_5m_input_tokens, 0)
            - coalesce(c.cache_creation_1h_input_tokens, 0), 0)
          ELSE 0 END) AS tok_1h
  FROM classified c
)
, recomputed AS (
  SELECT e.*,
    round(
      (coalesce(e.input_tokens, 0) * coalesce(e.in_rate, 0)
       + coalesce(e.output_tokens, 0) * coalesce(e.out_rate, 0)
       + e.tok_5m * coalesce(e.write5m_rate, 0)
       + e.tok_1h * coalesce(e.write1h_rate, 0)
       + coalesce(e.cache_read_input_tokens, 0) * coalesce(e.read_rate, 0))
      * coalesce(e.cost_multiplier, 1) * coalesce(e.group_cost_multiplier, 1)
    , ${COST_SCALE}) AS expected_cost,
    round(
      (coalesce(e.input_tokens, 0) * coalesce(
             CASE WHEN e.trigger_tokens > e.threshold_tokens AND e.prio_above_in IS NOT NULL
                  THEN e.prio_above_in ELSE coalesce(e.prio_base_in, e.in_rate) END, 0)
       + coalesce(e.output_tokens, 0) * coalesce(
             CASE WHEN e.trigger_tokens > e.threshold_tokens AND e.prio_above_out IS NOT NULL
                  THEN e.prio_above_out ELSE coalesce(e.prio_base_out, e.out_rate) END, 0)
       + e.tok_5m * coalesce(e.write5m_rate, 0)
       + e.tok_1h * coalesce(e.write1h_rate, 0)
       + coalesce(e.cache_read_input_tokens, 0) * coalesce(
             CASE WHEN e.trigger_tokens > e.threshold_tokens AND e.prio_above_read IS NOT NULL
                  THEN e.prio_above_read ELSE coalesce(e.prio_base_read, e.read_rate) END, 0))
      * coalesce(e.cost_multiplier, 1) * coalesce(e.group_cost_multiplier, 1)
    , ${COST_SCALE}) AS expected_cost_priority
  FROM expected e
)
SELECT json_build_object(
  'sampled', count(*),
  'comparable', count(*) FILTER (WHERE not_comparable IS NULL),
  'matched', count(*) FILTER (WHERE not_comparable IS NULL AND expected_cost = cost_usd),
  'matchedPriorityOnly', count(*) FILTER (
    WHERE not_comparable IS NULL AND expected_cost <> cost_usd AND expected_cost_priority = cost_usd),
  'mismatch', count(*) FILTER (
    WHERE not_comparable IS NULL AND expected_cost <> cost_usd AND expected_cost_priority <> cost_usd),
  'hasPriorityFields', count(*) FILTER (WHERE not_comparable IS NULL AND has_priority_fields),
  'notComparable', coalesce(
    (SELECT json_object_agg(not_comparable, n) FROM (
      SELECT not_comparable, count(*) AS n FROM recomputed
      WHERE not_comparable IS NOT NULL GROUP BY not_comparable ORDER BY n DESC
    ) t), '{}'::json),
  'samples', coalesce(
    (SELECT json_agg(x) FROM (
      SELECT id, price_model, cost_usd::text AS stored_cost_usd,
             expected_cost::text AS recomputed_cost_usd,
             expected_cost_priority::text AS recomputed_cost_usd_priority,
             in_rate::text AS input_rate, out_rate::text AS output_rate
      FROM recomputed
      WHERE not_comparable IS NULL AND expected_cost <> cost_usd AND expected_cost_priority <> cost_usd
      ORDER BY abs(expected_cost - cost_usd) DESC, id LIMIT 20
    ) x), '[]'::json)
) FROM recomputed`;
}

// ---------------------------------------------------------------------------
// 自检（不连库）：窗口解析 + 排除口径 + 判定映射
// ---------------------------------------------------------------------------
function selfTest() {
  const failures = [];
  const assert = (name, ok, detail = "") => {
    if (!ok) failures.push(`${name}${detail ? `：${detail}` : ""}`);
  };

  const now = new Date("2026-09-12T12:00:00Z");
  const rel = resolveWindow(readConfig(["--dry-run", "--window", "7d"]), now);
  assert("window/7d 跨度", rel.to - rel.from === 7 * 86_400_000);
  assert("window/7d 右端点是现在", rel.to.getTime() === now.getTime());
  const abs = resolveWindow(
    readConfig(["--dry-run", "--from", "2026-09-01T00:00:00Z", "--to", "2026-09-02T00:00:00Z"]),
    now
  );
  assert("from/to 保序", abs.from.toISOString() === "2026-09-01T00:00:00.000Z");
  assert("from/to 半开右端点", abs.to.toISOString() === "2026-09-02T00:00:00.000Z");
  let threw = false;
  try {
    resolveWindow(readConfig(["--dry-run", "--window", "7x"]), now);
  } catch {
    threw = true;
  }
  assert("非法窗口应报错", threw);
  threw = false;
  try {
    resolveWindow(readConfig(["--dry-run", "--from", "2026-09-02T00:00:00Z", "--to", "2026-09-01T00:00:00Z"]), now);
  } catch {
    threw = true;
  }
  assert("倒置区间应报错", threw);
  threw = false;
  try {
    readConfig(["--dry-run", "--bogus"]);
  } catch {
    threw = true;
  }
  assert("未知参数应报错", threw);

  // 排除口径：这三条与触发器 fn_upsert_usage_ledger 一一对应，写错会让对账静默漏行
  const sql = ledgerSql("2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z");
  assert("排除 warmup", sql.includes("<> 'warmup'"));
  assert("排除 count_tokens", sql.includes("'/v1/messages/count_tokens'"));
  assert("排除 compact", sql.includes("'/v1/responses/compact'"));
  assert("replay 账本记 0", sql.includes("CASE WHEN s.is_replay THEN 0::numeric"));
  assert("时间窗半开", sql.includes("created_at < w.hi"));

  // 判定映射：任一硬检查非零即 FAIL（exit 1）
  const bad = verdictOf({
    ledger: { missing: 0, divergent: 1 },
    breakdown: { totalMismatch: 0, baseMismatch: 0, cacheSplitMismatch: 0, costMismatch: 0, multiplierMismatch: 0 },
    price: { mismatch: 0 },
  }, false);
  assert("账本差异判 FAIL", bad.verdict === "FAIL" && bad.exitCode === 1);
  const softOnly = verdictOf({
    ledger: { missing: 0, divergent: 0 },
    breakdown: { totalMismatch: 0, baseMismatch: 0, cacheSplitMismatch: 0, costMismatch: 0, multiplierMismatch: 0 },
    price: { mismatch: 3 },
  }, false);
  assert("重算差异默认不判 FAIL", softOnly.verdict === "PASS" && softOnly.exitCode === 0);
  const strict = verdictOf({
    ledger: { missing: 0, divergent: 0 },
    breakdown: { totalMismatch: 0, baseMismatch: 0, cacheSplitMismatch: 0, costMismatch: 0, multiplierMismatch: 0 },
    price: { mismatch: 3 },
  }, true);
  assert("strict-price 下重算差异判 FAIL", strict.verdict === "FAIL" && strict.exitCode === 1);

  if (failures.length > 0) {
    process.stderr.write(`自检失败 ${failures.length} 项：\n`);
    for (const failure of failures) process.stderr.write(`  - ${failure}\n`);
    return 1;
  }
  process.stderr.write("自检通过（窗口解析、排除口径、判定映射）\n");
  return 0;
}

// verdictOf 汇总硬检查与软检查得到最终判定。
function verdictOf(report, strictPrice) {
  const hard =
    report.ledger.missing + report.ledger.divergent +
    report.breakdown.totalMismatch + report.breakdown.baseMismatch +
    report.breakdown.cacheSplitMismatch + report.breakdown.costMismatch +
    report.breakdown.multiplierMismatch;
  const soft = strictPrice ? report.price.mismatch : 0;
  const defects = hard + soft;
  return { verdict: defects === 0 ? "PASS" : "FAIL", exitCode: defects === 0 ? 0 : 1, defects };
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------
function usage() {
  return [
    "用量/成本对账（只读）",
    "",
    "用法：CCH_DSN=postgres://… node scripts/reconcile-usage.mjs [选项]",
    "",
    "选项：",
    "  --window <30m|24h|7d>   相对窗口（默认 24h，右端点为现在）",
    "  --from/--to <ISO 时间>   绝对窗口（半开区间，必须成对）",
    "  --strict-price           价目重算差异也算失败（默认只报不判）",
    "  --price-sample <n>       价目复算抽样行数（默认 200；先按每模型 ≤25 行保覆盖，再按 cost_usd 降序）",
    "  --dry-run                只打印窗口与计划，不连库",
    "  --self-test              不连库，自检窗口解析/排除口径/判定映射",
    "  --json                   机器可读报告（默认也输出 JSON，本项保留兼容）",
    "  --help                   本帮助",
    "",
    "退出码：0 无差异；1 有差异；2 配置或依赖错误",
  ].join("\n");
}

function main() {
  const config = readConfig(process.argv.slice(2));
  if (config.help) {
    process.stdout.write(`${usage()}\n`);
    process.exit(0);
  }
  if (config.selfTest) process.exit(selfTest());

  const window = resolveWindow(config);
  const fromIso = isoSeconds(window.from);
  const toIso = isoSeconds(window.to);
  const plan = {
    window: { label: window.label, from: fromIso, to: toIso, halfOpen: "[from, to)" },
    priceSample: config.priceSample,
    strictPrice: config.strictPrice,
  };
  if (config.dryRun) {
    process.stdout.write(`${JSON.stringify({ dryRun: true, plan }, null, 2)}\n`);
    process.exit(0);
  }

  const db = parseDsn(config.dsn);
  const ledger = psqlJson(db, ledgerSql(fromIso, toIso));
  const breakdown = psqlJson(db, breakdownSql(fromIso, toIso));
  const price = psqlJson(db, priceSql(fromIso, toIso, config.priceSample));

  const report = { plan, ledger, breakdown, price, ...verdictOf({ ledger, breakdown, price }, config.strictPrice) };
  process.stderr.write(`窗口：${window.label}（${fromIso} .. ${toIso}）\n`);
  process.stderr.write(
    `账本：${report.ledger.rows} 行，缺 ${report.ledger.missing}，差异 ${report.ledger.divergent}` +
      `（请求侧合计 ${report.ledger.requestTotal} / 账本侧合计 ${report.ledger.ledgerTotal}）\n`
  );
  process.stderr.write(
    `breakdown：${report.breakdown.rows} 行，乘积 ${report.breakdown.totalMismatch}，基数 ${report.breakdown.baseMismatch}，` +
      `缓存拆分 ${report.breakdown.cacheSplitMismatch}，cost_usd ${report.breakdown.costMismatch}，倍率列 ${report.breakdown.multiplierMismatch}\n`
  );
  process.stderr.write(
    `价目重算：抽样 ${report.price.sampled}，可比 ${report.price.comparable}，命中 ${report.price.matched}` +
      `（其中仅 priority 模式命中 ${report.price.matchedPriorityOnly}），差异 ${report.price.mismatch}\n` +
      `  不可比 ${JSON.stringify(report.price.notComparable)}${report.strictPrice ? "" : "（默认不判失败）"}\n`
  );
  process.stderr.write(`判定：${report.verdict}\n`);
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  process.exit(report.exitCode);
}

try {
  main();
} catch (error) {
  if (error instanceof ConfigError) process.stderr.write(`配置错误：${error.message}\n`);
  else process.stderr.write(`致命错误：${error?.message ?? error}\n`);
  process.exit(2);
}
