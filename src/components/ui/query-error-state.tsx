"use client";

import { useTranslations } from "next-intl";
import { Button } from "@/components/ui/button";

interface QueryErrorStateProps {
  /** 失败文案（i18n key 已翻译好的文本，调用方提供，避免本组件绑定命名空间）。 */
  message: string;
  onRetry: () => void;
  className?: string;
}

/**
 * 客户端取数失败的内联错误态 + 重试按钮。
 *
 * 与 `dashboard-bento-sections.tsx` 的既有写法同一形态（destructive 描边 + 重试）；
 * 抽出来是因为静态化后每个页面都要有这一态，逐页复制六行 JSX 只会产生 14 份近似实现。
 */
export function QueryErrorState({ message, onRetry, className }: QueryErrorStateProps) {
  const t = useTranslations("common");

  return (
    <div
      className={
        className ??
        "flex items-center justify-between gap-3 border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive"
      }
    >
      <p>{message}</p>
      <Button variant="outline" size="sm" onClick={onRetry}>
        {t("retry")}
      </Button>
    </div>
  );
}
