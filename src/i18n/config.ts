/**
 * i18n Configuration
 * Defines supported locales and default locale for the application
 */

// Supported locales in the application.
//
// 只留简体中文与英文：`zh-TW`/`ru`/`ja` 已退役（词表删除、路由不再生成）。
// 退役语种的旧 URL 由 `scripts/build-ui-export.mjs` 生成的**重定向壳**接住并跳到默认语言，
// 见该脚本的 retiredLocales 与 retired-locale.ts 的 retiredLocaleRedirectHtml。
export const locales = ["zh-CN", "en"] as const;

// TypeScript type for locale
export type Locale = (typeof locales)[number];

// Default locale (Chinese Simplified)
export const defaultLocale: Locale = "zh-CN";

// Locale cookie shared by next-intl middleware and app-level routing helpers
export const localeCookieName = "NEXT_LOCALE";

// Locale labels for language switcher UI
export const localeLabels: Record<Locale, string> = {
  "zh-CN": "简体中文",
  en: "English",
};

// Locale names in English (for metadata, SEO)
export const localeNamesInEnglish: Record<Locale, string> = {
  "zh-CN": "Chinese (Simplified)",
  en: "English",
};

// Retired locales：曾经注册、现已退役的语种。
//
// 保留这份清单的唯一用途是「旧 URL 不失效」：静态导出会为每个退役前缀生成重定向壳
// （见 scripts/build-ui-export.mjs），而这种「曾是合法前缀」的信息无法从 locales 反推。
// 不要把它当可服务语种用——它既不参与路由，也不参与词表装配。
export const retiredLocales = ["zh-TW", "ru", "ja"] as const;
