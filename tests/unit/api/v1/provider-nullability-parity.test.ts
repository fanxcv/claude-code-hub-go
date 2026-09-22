import { readFileSync } from "node:fs";
import { join } from "node:path";
import { toJSONSchema } from "zod";
import { describe, expect, it } from "vitest";
import { ProviderCreateSchema, ProviderUpdateSchema } from "@/lib/api/v1/schemas/providers";

/**
 * provider 写路径**可空性**的跨语言对拍：Node 的 Zod schema 是唯一真源。
 *
 * 事故（生产 2026-09-13）：用户编辑供应商保存报
 * `{"path":["max_retry_attempts"],"code":"invalid_type","message":"Expected number, received null"}`，
 * 新增供应商报同样一句 `One or more fields are invalid`。
 * 根因：Node 是 `maxRetryAttempts: z.number().int().min(1).max(10).nullable().optional()`，
 * 而 Go 的字段表误用了非空 spec（`providerIntFieldSpec`）→ null 被判 invalid_type。
 *
 * **为什么 null 是合法语义（UI 出处，逐条实测）**：
 *   - `provider-form-types.ts:107`   `maxRetryAttempts: number | null`
 *   - `provider-form-context.tsx:409` 初始值 `maxRetryAttempts: null`
 *   - `provider-form-context.tsx:496` 载入已有供应商 `sourceProvider?.maxRetryAttempts ?? null`
 *   - `provider-form-context.tsx:313-315` 仅在「各端点一致（uniform）」时填数值，否则**保持 null**
 * 即 null 表示「未设 / 各端点不一致」，对应 DB 的 `max_retry_attempts`（可空、无默认值）。
 * 对照：同一对象里 `openDurationMinutes` / `halfOpenSuccessThreshold` 是 `number | undefined`
 * ——那两个列是 NOT NULL，UI 用「省略」而不是 null。**UI 类型与列可空性一一对应**。
 *
 * 做法：**运行时内省** `toJSONSchema`（不靠正则猜 schema），取出每个字段是否含 `{type:"null"}`；
 * 再从 Go 字段表源码解出该字段用的 spec 函数名；两侧比对。解析不到就**失败**，绝不静默跳过。
 */

const GO_FIELDS_FILE = join(process.cwd(), "go/internal/adminapi/providers_write_fields.go");

/** 接受 null 的 spec 函数（Go 侧）。 */
const NULLABLE_SPECS = new Set([
  "providerNullableIntFieldSpec",
  "providerNullableFieldSpec",
  "providerNumericFieldSpec",
  "providerJSONFieldSpec",
  // 低速系数专用的 0-1 小数值规格：同样接 null（null = 取代码默认 0.3），只是多了上界 1。
  "providerSlowRateRatioFieldSpec",
]);

/**
 * **例外类**：Node 收 null，但对应 DB 列是 NOT NULL，故 Go 把 null 落成默认值 0
 * （Node 创建时是 `providerData.<field> ?? PROVIDER_TIMEOUT_DEFAULTS.*`，三个默认值都是 0）。
 * 这条**必须逐字段列出**：将来谁把某个列改成可空、或把某个字段从这张表里挪走，都要在这里显式发生。
 */
const NULL_TO_DEFAULT_SPECS: Record<string, string> = {
  first_byte_timeout_streaming_ms: "NOT NULL DEFAULT 0（Node 创建时 ?? 0）",
  streaming_idle_timeout_ms: "NOT NULL DEFAULT 0（Node 创建时 ?? 0）",
  request_timeout_non_streaming_ms: "NOT NULL DEFAULT 0（Node 创建时 ?? 0）",
};

/**
 * **已存在的「Go 比 Node 宽」清单**——本钉子只钉住现状，不借机收紧（收紧会制造新的 400）。
 * 每条都要有理由；将来谁**新增**一条更宽的字段，钉子会红，逼出一次显式决定。
 */
const LENIENT_BY_DESIGN: Record<string, string> = {
  limit_concurrent_sessions:
    "Node 是 .optional() 非可空，但列可空且默认 0；UI 走省略路径，维持现状不收紧（改严会新增 400 风险）",
  cost_multiplier:
    "Node 是 .min(0).optional() 非可空，但列可空且默认 1.0；放宽不会报错，收紧反而会拒掉列能存的 null",
};
const LENIENT_PREFIXES: Array<{ prefix: string; reason: string }> = [
  {
    prefix: "codex_",
    reason:
      "Node 是 z.string().optional()（非可空），Go 用 providerNullableFieldSpec；Go 更宽，维持现状",
  },
  {
    prefix: "anthropic_",
    reason: "同上（Node 偏好类字段非可空，Go 允许 null），维持现状",
  },
  {
    prefix: "openai_",
    reason: "同上（Node 偏好类字段非可空，Go 允许 null），维持现状",
  },
  {
    prefix: "gemini_",
    reason: "同上（Node 偏好类字段非可空，Go 允许 null），维持现状",
  },
  { prefix: "cache_ttl_preference", reason: "同上（Node 非可空，Go 允许 null）" },
];

/** name / url / key 的校验在 providerValidateCreate 里（跨字段与格式判定），表里用 raw 解码占位。 */
const HANDLED_ELSEWHERE = new Set(["name", "url", "key"]);

type JsonSchema = {
  properties?: Record<string, JsonSchema>;
  anyOf?: JsonSchema[];
  type?: string | string[];
};

function isNullable(schema: JsonSchema | undefined): boolean {
  if (!schema) return false;
  if (Array.isArray(schema.anyOf) && schema.anyOf.some((branch) => branch.type === "null")) {
    return true;
  }
  const type = schema.type;
  return Array.isArray(type) ? type.includes("null") : type === "null";
}

function numericFieldNames(schema: JsonSchema): string[] {
  const props = schema.properties ?? {};
  return Object.keys(props).filter((name) => {
    const field = props[name];
    const branches = [field, ...(field.anyOf ?? [])];
    return branches.some((branch) => branch.type === "integer" || branch.type === "number");
  });
}

/** 从 Go 字段表源码解出 `字段名 → spec 函数名`。 */
function readGoSpecs(): Map<string, string> {
  const source = readFileSync(GO_FIELDS_FILE, "utf8");
  const start = source.indexOf("func providerCreateWriteSpecs()");
  const end = source.indexOf("// providerUpdateWriteSpecs", start);
  if (start < 0 || end < 0) {
    throw new Error(
      `未能定位 Go 字段表（providerCreateWriteSpecs/…UpdateWriteSpecs）：${GO_FIELDS_FILE}`
    );
  }
  const region = source.slice(start, end);
  const specs = new Map<string, string>();
  // 形如：  "max_retry_attempts": providerNullableIntFieldSpec(&minRetry, &maxRetry),
  const pattern = /"([a-z0-9_]+)":\s*(?:providerDecodeSpec\{Decode:\s*)?([A-Za-z]\w*)\s*[({]/g;
  for (const match of region.matchAll(pattern)) {
    specs.set(match[1], match[2]);
  }
  return specs;
}

const createSchema = toJSONSchema(ProviderCreateSchema as never) as JsonSchema;
const updateSchema = toJSONSchema(ProviderUpdateSchema as never) as JsonSchema;
const goSpecs = readGoSpecs();

describe("provider 写路径可空性：Node schema ↔ Go 字段表", () => {
  it("解析面健康：两侧都取到了足够样本（防空跑）", () => {
    // 少于这些数字说明解析路径坏了（改了函数名/文件位置），此时「全绿」毫无意义。
    expect(goSpecs.size).toBeGreaterThan(50);
    const names = numericFieldNames(createSchema);
    expect(names.length).toBeGreaterThan(15);
    expect(names).toContain("max_retry_attempts");
  });

  it("Node 可空的数值字段，Go 必须接受 null（本次事故的类）", () => {
    const createNullable = numericFieldNames(createSchema).filter((name) =>
      isNullable(createSchema.properties?.[name])
    );
    const updateNullable = numericFieldNames(updateSchema).filter((name) =>
      isNullable(updateSchema.properties?.[name])
    );
    // 创建与更新两条路径都必须可空（真实事故两条都撞到）。
    expect(createNullable.length).toBeGreaterThan(0);
    expect(new Set(updateNullable)).toEqual(new Set(createNullable));

    const violations: string[] = [];
    for (const name of createNullable) {
      const spec = goSpecs.get(name);
      if (!spec) {
        // 字段没进 Go 表 = 根本没校验；这里必须显式失败，不能当成「没事」。
        violations.push(`${name}: Go 字段表里没有这个字段`);
        continue;
      }
      if (NULLABLE_SPECS.has(spec)) continue;
      if (NULL_TO_DEFAULT_SPECS[name]) {
        // 例外类：列 NOT NULL，null 落默认值；必须用「把 null 落 0」的那种 spec。
        if (spec !== "providerTimeoutFieldSpec") {
          violations.push(
            `${name}: 列 NOT NULL 应用 providerTimeoutFieldSpec（null→0），实际 ${spec}`
          );
        }
        continue;
      }
      violations.push(`${name}: Node 可空而 Go 的 spec 是 ${spec}（不接受 null）`);
    }
    expect(violations, violations.join("\n")).toEqual([]);
  });

  it("Go 更宽的地方必须逐条登记（新增更宽字段会红）", () => {
    const props = createSchema.properties ?? {};
    const unregistered: string[] = [];
    for (const [name, spec] of goSpecs) {
      if (HANDLED_ELSEWHERE.has(name)) continue;
      if (!NULLABLE_SPECS.has(spec)) continue;
      if (isNullable(props[name])) continue; // 两侧一致，正常
      if (LENIENT_BY_DESIGN[name]) continue;
      if (LENIENT_PREFIXES.some((entry) => name.startsWith(entry.prefix))) continue;
      // Go 独有字段（不在 Node schema 里）不算「比 Node 宽」。
      if (!(name in props)) continue;
      unregistered.push(`${name}: Node 非可空而 Go 用 ${spec}（若有意，请登记理由）`);
    }
    expect(unregistered, unregistered.join("\n")).toEqual([]);
  });

  it("UI 契约：表单确实会送 null（改 UI 或改 Go 都会让本钉子红）", () => {
    const types = readFileSync(
      join(
        process.cwd(),
        "src/app/[locale]/settings/providers/_components/forms/provider-form/provider-form-types.ts"
      ),
      "utf8"
    );
    const context = readFileSync(
      join(
        process.cwd(),
        "src/app/[locale]/settings/providers/_components/forms/provider-form/provider-form-context.tsx"
      ),
      "utf8"
    );
    // 类型允许 null、初始值是 null、载入时 ?? null、非 uniform 时保持 null —— 四者缺一说明
    // 「UI 会送 null」这个前提变了，必须回来重新决定 Go 侧的可空性（而不是让钉子静默失效）。
    expect(types).toMatch(/maxRetryAttempts:\s*number\s*\|\s*null/);
    expect(context).toMatch(/maxRetryAttempts:\s*null/);
    expect(context).toMatch(/sourceProvider\?\.maxRetryAttempts\s*\?\?\s*null/);
    expect(context).toMatch(/maxRetryAttempts[\s\S]{0,160}?:\s*null/);
    // 反面：对应 NOT NULL 列的两个字段必须用 undefined（省略）而不是 null。
    expect(types).toMatch(/openDurationMinutes:\s*number\s*\|\s*undefined/);
    expect(types).toMatch(/halfOpenSuccessThreshold:\s*number\s*\|\s*undefined/);
  });
});
