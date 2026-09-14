/**
 * 无需 zod 的环境变量读取面。
 *
 * 为什么单独一个模块：`env.schema.ts` 为校验整份环境变量依赖 zod，而 zod（v4 核心及其内联依赖，
 * 实测 376 KiB 解码 / 84 KiB br）一旦被**共享**模块引用，就会落进 66/72 个路由的共享 chunk——
 * 客户端为了「读一个布尔/一个字符串」付出整个校验库。本文件只读标量，不引入任何依赖。
 *
 * 取值必须与 `env.schema.ts` 的同名项一致：`tests/unit/config/env-flags.test.ts` 把两侧钉在一起，
 * 改任一侧都会让该测试转红。
 */

/** `NODE_ENV` 未设时的取值，与 EnvSchema 的 `NODE_ENV` 默认值一致。 */
export const DEFAULT_NODE_ENV = "development";

/** `TZ` 未设时的取值，与 EnvSchema 的 `TZ` 默认值一致。 */
export const DEFAULT_TIMEZONE = "Asia/Shanghai";

/**
 * 是否开发环境。
 *
 * 与 `getEnvConfig().NODE_ENV === "development"` 同义：schema 侧对未设值取 enum 默认值，
 * 此处对未设值取同一默认值。
 */
// 本函数曾经只定义在 config/env.schema.ts（zod 环境校验器）里，于是「读一个布尔」也要把整份
// zod v4（压缩前 412 KB）拖进引用方；而唯一调用者是 logger，logger 又被客户端与多个共享模块
// 引用 ⇒ zod 成了共享 chunk，在**每个**页面（含 /login、/status 这类不需要校验的公开页）下发。
// 故本模块刻意不引 zod：它是最小依赖面。
//
// 与 EnvSchema 那份的**唯一语义差别**：EnvSchema 会把非法的 NODE_ENV 变成校验异常，这里按
// 「非 development 即非开发环境」处理、不抛——对 logger 的用途（是否开 pretty 传输）才是想要的。
export function isDevelopment(): boolean {
  return (process.env.NODE_ENV ?? DEFAULT_NODE_ENV) === "development";
}

/**
 * 时区候选（**未校验**，调用方仍须过 `isValidIANATimezone`）。
 *
 * 与 `getEnvConfig().TZ` 同义：未设时落默认值；空串原样返回（schema 的 `z.string()` 接受空串，
 * 而调用方按真值判断，故两侧同样落到 UTC 兜底）。
 */
export function envTimezone(): string {
  return process.env.TZ === undefined ? DEFAULT_TIMEZONE : process.env.TZ;
}
