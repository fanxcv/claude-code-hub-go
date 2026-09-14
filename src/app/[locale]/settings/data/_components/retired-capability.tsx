"use client";

import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";

/**
 * 已退役能力的原位说明。
 *
 * 为什么不用「置灰的按钮」：灰按钮会让人以为「条件满足了就能点」，而这两个能力是**永久下线**
 * （随 Node 后端退役：导出依赖 Node 运行时内置的 pg_dump、导入依赖 psql 且属破坏性操作，
 * 详见）。显示一个可按但必然失败的控件，
 * 比明确说明「已不可用」更坏——前者会让人以为是自己操作错了。
 *
 * 故这里只渲染说明，不渲染任何控件；备份路径在 i18n 文案里给全（直连 PG 用宿主工具）。
 */
export function RetiredCapability({ capability }: { capability: "export" | "import" }) {
  const t = useTranslations("settings.data.retired");

  return (
    <div
      data-slot="retired-capability"
      className="flex items-start gap-3 rounded-lg border border-amber-300/60 bg-amber-50/60 p-4 text-sm dark:border-amber-900/60 dark:bg-amber-950/30"
    >
      <AlertCircle
        className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400"
        aria-hidden="true"
      />
      <div className="space-y-1">
        <p className="font-medium text-amber-900 dark:text-amber-200">{t("title")}</p>
        <p className="text-muted-foreground">{t(`${capability}Description`)}</p>
        <p className="text-muted-foreground">{t("backupHint")}</p>
      </div>
    </div>
  );
}
