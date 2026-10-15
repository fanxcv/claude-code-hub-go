/**
 * @vitest-environment happy-dom
 */

import { useLocale, useTranslations } from "next-intl";
import { act, Suspense } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, test } from "vitest";
import { loadClientMessages } from "@/i18n/client-messages";
import { I18nProvider } from "./i18n-provider";

/**
 * 词表改成按 locale `import()`（每种语言一份独立 chunk）之后，取用点仍必须拿到**当前语言**：
 * 拿错语言、或拿不到而回落成键名（如 `common.save`），都在这里现形。
 * 服务端一侧由导出产物钉住（每语种 html 的 `lang` 与产物体积），这里钉客户端渲染路径。
 */

function Probe() {
  const t = useTranslations("common");
  return (
    <span>
      {useLocale()}:{t("save")}
    </span>
  );
}

describe("I18nProvider 按 locale 取词表", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  test.each([
    ["en", "Save"],
    ["zh-CN", "保存"],
  ])("%s 取到本语言词表", async (locale, expected) => {
    const container = document.createElement("div");
    document.body.appendChild(container);
    const root = createRoot(container);

    await act(async () => {
      root.render(
        <Suspense fallback={<span data-testid="pending" />}>
          <I18nProvider locale={locale} timeZone="UTC">
            <Probe />
          </I18nProvider>
        </Suspense>
      );
    });
    // provider 内部 `use()` 的那份 promise（同一 locale 复用同一实例）：等它落地即等词表到齐。
    await act(async () => {
      await loadClientMessages(locale);
    });

    expect(container.textContent).toBe(`${locale}:${expected}`);
    await act(async () => {
      root.unmount();
    });
  });
});
