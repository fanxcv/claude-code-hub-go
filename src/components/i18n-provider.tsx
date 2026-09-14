"use client";

import { NextIntlClientProvider } from "next-intl";
import type { ReactNode } from "react";
import { getClientMessages } from "@/i18n/client-messages";

interface I18nProviderProps {
  locale: string;
  children: ReactNode;
  /**
   * 服务端解析出的时区与稳定 now（短标量，进 payload 无代价）。
   * 不传时保持 next-intl 默认语义（客户端按浏览器时区），与静态导出壳的既有行为一致。
   */
  timeZone?: string;
  now?: Date;
}

/**
 * 客户端词表供给点。
 *
 * 词表**在本组件内部**按 locale 取用（见 `@/i18n/client-messages`），而不是由服务端 layout
 * 以 props 传入：props 会跨 server/client 边界序列化进每个路由的 RSC flight payload，
 * 实测造成全站约 88 MiB 的词表重复（每路由每语言 3 份 payload + 1 份 HTML 内联）。
 *
 * 静态 import 保证词表在预渲染期同步可用——本仓有依赖预渲染产出的本地化文案
 * （usage-doc 约 156 KiB），改成异步加载会丢文案并造成 hydration 不一致。
 */
export function I18nProvider({ locale, timeZone, now, children }: I18nProviderProps) {
  return (
    <NextIntlClientProvider
      locale={locale}
      messages={getClientMessages(locale)}
      timeZone={timeZone}
      now={now}
    >
      {children}
    </NextIntlClientProvider>
  );
}
