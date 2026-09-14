"use client";

import { useTranslations } from "next-intl";
import { useMemo } from "react";

export type SettingsNavIconName =
  | "settings"
  | "activity"
  | "dollar-sign"
  | "server"
  | "shield-alert"
  | "alert-triangle"
  | "filter"
  | "smartphone"
  | "database"
  | "file-text"
  | "bell";

export interface SettingsNavItem {
  href: string;
  label: string;
  labelKey?: string;
  iconName?: SettingsNavIconName;
}

// Static navigation items for navigation structure
export const SETTINGS_NAV_ITEMS: SettingsNavItem[] = [
  {
    href: "/settings/config",
    labelKey: "nav.config",
    label: "Configuration",
    iconName: "settings",
  },
  {
    href: "/settings/status-page",
    labelKey: "nav.statusPage",
    label: "Status Page",
    iconName: "activity",
  },
  { href: "/settings/prices", labelKey: "nav.prices", label: "Prices", iconName: "dollar-sign" },
  {
    href: "/settings/providers",
    labelKey: "nav.providers",
    label: "Providers",
    iconName: "server",
  },
  {
    href: "/settings/sensitive-words",
    labelKey: "nav.sensitiveWords",
    label: "Sensitive Words",
    iconName: "shield-alert",
  },
  {
    href: "/settings/error-rules",
    labelKey: "nav.errorRules",
    label: "Error Rules",
    iconName: "alert-triangle",
  },
  {
    href: "/settings/request-filters",
    labelKey: "nav.requestFilters",
    label: "Request Filters",
    iconName: "filter",
  },
  {
    href: "/settings/client-versions",
    labelKey: "nav.clientVersions",
    label: "Client Versions",
    iconName: "smartphone",
  },
  { href: "/settings/data", labelKey: "nav.data", label: "Data", iconName: "database" },
  { href: "/settings/logs", labelKey: "nav.logs", label: "Logs", iconName: "file-text" },
  {
    href: "/settings/notifications",
    labelKey: "nav.notifications",
    label: "Notifications",
    iconName: "bell",
  },
  {
    href: "/dashboard/audit-logs",
    labelKey: "nav.auditLogs",
    label: "Audit Logs",
    iconName: "file-text",
  },
];

// 取译文后的导航项。
//
// 为何是客户端 hook 而非服务端函数：`settings/layout.tsx` 原以 `await getTranslations(...)` 取译文，
// 使该 layout 被 `scripts/build-ui-export.mjs` 判为服务端绑定而移出导出——连同它渲染的站点头部
// 一起从产物里消失。改为 hook 后，文案取自 `I18nProvider` 的词表（全站共享 chunk），
// 与 `DASHBOARD` 头部的做法一致。
export function useTranslatedNavItems(): SettingsNavItem[] {
  const t = useTranslations("settings");
  return useMemo(
    () =>
      SETTINGS_NAV_ITEMS.map((item) => ({
        ...item,
        label: item.labelKey ? t(item.labelKey) : item.label,
      })),
    [t]
  );
}
