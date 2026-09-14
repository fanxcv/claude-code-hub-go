import en from "@messages/en";
import zhCN from "@messages/zh-CN";
import type { NextIntlClientProvider } from "next-intl";
import type { ComponentProps } from "react";
import type { Locale } from "./config";

/**
 * next-intl provider 实际接受的词表类型。
 * 不用 `AbstractIntlMessages`：本仓词表含数组值（如 `usage` 的步骤列表），会被它判为不合。
 */
type ClientMessages = NonNullable<ComponentProps<typeof NextIntlClientProvider>["messages"]>;

/**
 * 客户端词表来源（每 locale 一份完整 catalog）。
 *
 * 为什么不从服务端 layout 以 props 传 `messages`：
 * 跨 server/client 边界的 props 会被序列化进**每个路由、每个 locale** 的 RSC flight payload。
 * 实测 `zh-CN/dashboard` 的 payload 里词表占 65%（178 KiB / 275 KiB），而该 payload 每路由
 * 存 3 份（`index.txt` / `__next._full.txt` / `__next.$d$locale.txt`）另有 HTML 内联一份，
 * 于是全站仅词表就占约 88 MiB。改为在客户端模块内引用后，词表只作为共享 JS chunk 存在
 * （全站一份），不再进 payload。
 *
 * 为什么是静态 `import` 而非动态 `import()`：
 * 本项目有多处（如 usage-doc 的约 156 KiB）**预渲染**产出的本地化文案，需要词表在预渲染期
 * 同步可用；动态导入会让首帧拿不到词表，既丢文案又造成 hydration 不一致。
 */
const catalogs: Record<Locale, ClientMessages> = {
  "zh-CN": zhCN,
  en,
};

/** 取某 locale 的完整词表；未知 locale 回落到默认语言。 */
export function getClientMessages(locale: string): ClientMessages {
  return catalogs[locale as Locale] ?? catalogs["zh-CN"];
}
