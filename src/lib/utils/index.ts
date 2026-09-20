/**
 * 工具函数统一导出
 */

// 样式相关
export { cn } from "./cn";
export type { CurrencyCode } from "./currency";
// 金额工具（`currency`）**有意不在此 re-export**：
//
// `currency.ts` 引 `decimal.js-light`（`Decimal` 是值导出），而本 barrel 被大量
// 全站 UI 组件引用（多数只为取 `cn` 这类零依赖工具，如 `ui/theme-switcher.tsx`）。
// 在 barrel 里转出金额函数，会让**整条依赖图**（含 decimal.js-light）进入每个页面的
// 客户端 chunk——实测曾因此在 66/73 个路由上多下约 50 KiB br（登录页也中招）。
// 需要金额能力的模块请直连 `@/lib/utils/currency`（`calculateRequestCost` 同理直连）。
// SSE 处理
export { parseSSEData } from "./sse";
export { formatTokenAmount } from "./token";
// 验证和格式化
export {
  clampIntInRange,
  clampTpm,
  clampWeight,
  formatTpmDisplay,
  isValidUrl,
  maskKey,
  validateNumericField,
  validatePositiveDecimalField,
} from "./validation";
