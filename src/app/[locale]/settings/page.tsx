"use client";

import { useEffect } from "react";
import { useRouter } from "@/i18n/routing";
import { SETTINGS_NAV_ITEMS } from "./_lib/nav-items";

/**
 * 设置首页只做一次跳转：落到导航里的第一项。
 *
 * 改造前是服务端 `redirect()`（`@/i18n/routing`）；静态导出后没有服务端跳转，
 * 改由客户端 `router.replace` 完成，落点与 `SETTINGS_NAV_ITEMS` 首项完全一致。
 */
export default function SettingsIndex() {
  const router = useRouter();
  const href = SETTINGS_NAV_ITEMS[0]?.href ?? "/dashboard";

  useEffect(() => {
    router.replace(href);
  }, [href, router]);

  return null;
}
