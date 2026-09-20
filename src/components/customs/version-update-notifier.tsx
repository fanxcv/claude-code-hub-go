"use client";

import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";
import { useEffect, useState } from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

interface VersionUpdate {
  current: string | null;
  latest: string | null;
  releaseUrl: string;
}

/** 只有 hasUpdate 为真且 releaseUrl 非空才算「有新版本」——缺地址宁可不错。 */
function parseVersionUpdate(payload: unknown): VersionUpdate | null {
  if (typeof payload !== "object" || payload === null) {
    return null;
  }

  const { hasUpdate, releaseUrl, current, latest } = payload as Record<string, unknown>;

  if (hasUpdate !== true || typeof releaseUrl !== "string" || releaseUrl === "") {
    return null;
  }

  return {
    current: typeof current === "string" ? current : null,
    latest: typeof latest === "string" ? latest : null,
    releaseUrl,
  };
}

/**
 * 顶栏的升级提示：检测到新版本时渲染一个跳转 release 页的图标，否则不渲染任何节点。
 *
 * 数据取自本进程的 `GET /api/version`（Go 侧已有 5 分钟缓存与 GitHub 回退链），浏览器不直连外网。
 */
export function VersionUpdateNotifier() {
  const t = useTranslations("customs.version");
  const [update, setUpdate] = useState<VersionUpdate | null>(null);

  useEffect(() => {
    let active = true;

    void fetch("/api/version")
      .then((response) => response.json() as Promise<unknown>)
      .then((payload) => {
        if (!active) {
          return;
        }

        setUpdate(parseVersionUpdate(payload));
      })
      .catch(() => {});

    return () => {
      active = false;
    };
  }, []);

  if (!update) {
    return null;
  }

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <a
          href={update.releaseUrl}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={t("ariaUpdateAvailable")}
          className="inline-flex size-9 items-center justify-center rounded-full border border-border/60 bg-card/70 text-amber-600 shadow-xs transition-all duration-200 hover:border-border hover:bg-accent/60 dark:text-amber-400"
        >
          <AlertCircle className="size-4" aria-hidden="true" />
        </a>
      </TooltipTrigger>
      <TooltipContent>
        <p>{t("updateAvailable")}</p>
        {update.current && update.latest ? (
          <p className="font-mono">
            {update.current} → {update.latest}
          </p>
        ) : null}
      </TooltipContent>
    </Tooltip>
  );
}
