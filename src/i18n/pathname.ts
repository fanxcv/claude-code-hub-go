import { type Locale, locales, retiredLocales } from "./config";

const DEFAULT_INTERNAL_PATH = "/dashboard";
const PROTOCOL_LIKE_PATTERN = /^[a-zA-Z][a-zA-Z\d+.-]*:/;

function isLocale(value: string): value is Locale {
  return locales.some((locale) => locale === value);
}

/**
 * 是否是「语种形」的段，用于**剥离**而非取值。
 *
 * 包含退役语种：旧的 `/ja/dashboard` 链接仍可能以 query（如登录回跳的 `?redirect=`）的形式
 * 进入本模块，若不剥离就会拼出 `/en/ja/dashboard` 这种不存在的路径。
 * 反例说明为何不复用 isLocale：getLocaleFromValue 用它**取值**，放退役语种进去会把
 * 已退役的语言当成可切换目标。
 */
function isStrippableLocale(value: string): boolean {
  return isLocale(value) || retiredLocales.some((locale) => locale === value);
}

function normalizeFallback(fallback: string): string {
  const candidate = fallback.trim();

  if (!candidate?.startsWith("/") || candidate.startsWith("//")) {
    return DEFAULT_INTERNAL_PATH;
  }

  return candidate === "/" ? DEFAULT_INTERNAL_PATH : candidate;
}

export function getLocaleFromValue(value: string | null | undefined): Locale | null {
  if (!value) return null;

  const candidate = value.trim();
  return isLocale(candidate) ? candidate : null;
}

export function normalizePathnameForLocaleNavigation(
  pathname: string | null | undefined,
  fallback = DEFAULT_INTERNAL_PATH
): string {
  const safeFallback = normalizeFallback(fallback);
  const candidate = pathname?.trim() ?? "";

  if (!candidate?.startsWith("/") || candidate.startsWith("//")) {
    return safeFallback;
  }

  if (PROTOCOL_LIKE_PATTERN.test(candidate) || PROTOCOL_LIKE_PATTERN.test(candidate.slice(1))) {
    return safeFallback;
  }

  const suffixStart = candidate.search(/[?#]/);
  let path = suffixStart === -1 ? candidate : candidate.slice(0, suffixStart);
  const suffix = suffixStart === -1 ? "" : candidate.slice(suffixStart);

  while (true) {
    const localeMatch = path.match(/^\/([^/]+)(?=\/|$)/);
    const locale = localeMatch?.[1];

    if (!locale || !isStrippableLocale(locale)) {
      break;
    }

    path = path.slice(locale.length + 1) || "/";
  }

  if (path === "/") {
    return `${safeFallback}${suffix}`;
  }

  if (!path.startsWith("/") || path.startsWith("//") || PROTOCOL_LIKE_PATTERN.test(path.slice(1))) {
    return safeFallback;
  }

  return `${path}${suffix}`;
}
