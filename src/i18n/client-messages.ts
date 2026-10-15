import type { NextIntlClientProvider } from "next-intl";
import type { ComponentProps } from "react";
import { defaultLocale, type Locale, locales } from "./config";

/**
 * next-intl provider 实际接受的词表类型。
 * 不用 `AbstractIntlMessages`：本仓词表含数组值（如 `usage` 的步骤列表），会被它判为不合。
 */
type ClientMessages = NonNullable<ComponentProps<typeof NextIntlClientProvider>["messages"]>;

/**
 * 客户端词表：按 locale 各拆一份 async chunk。
 *
 * 这里曾是两份静态 `import`（`@messages/zh-CN` + `@messages/en`）：两份完整 catalog 落进同一个
 * eager chunk（实测 raw 577,580 B / gzip 176,173 B），被 66/73 个导出 html 引用（含 `/en/login`）
 * ——首屏白带另一种语言。改成按 locale `import()` 后，每种语言各一份 chunk，单次首屏只取当前语言。
 *
 * 词表在**预渲染期**仍需同步可用（否则首帧丢文案），所以取用点（`@/components/i18n-provider`）
 * 用 `use()` 挂住：服务端预渲染会等这份本地 import 落地（无网络），客户端 hydration 先取到当前
 * 语言的 chunk，两侧是同一份词表，不会语言回退。
 */
const loaders: Record<Locale, () => Promise<ClientMessages>> = {
  "zh-CN": () => import("@messages/zh-CN").then((module) => module.default),
  en: () => import("@messages/en").then((module) => module.default),
};

/**
 * 每个 locale 只 import 一次：`use()` 要求 promise 身份稳定，否则每次渲染都重新挂起。
 * （catalog 缺失属构建期错误，失败态无需重置——重置只会把同一个错误再抛一遍。）
 */
const pendingByLocale = new Map<Locale, Promise<ClientMessages>>();

/** 取某 locale 的完整词表；未知 locale 回落到默认语言。 */
export function loadClientMessages(locale: string): Promise<ClientMessages> {
  const key: Locale = locales.includes(locale as Locale) ? (locale as Locale) : defaultLocale;
  let pending = pendingByLocale.get(key);
  if (!pending) {
    pending = loaders[key]();
    pendingByLocale.set(key, pending);
  }
  return pending;
}
