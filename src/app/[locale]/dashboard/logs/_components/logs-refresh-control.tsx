"use client";

import { Radio, Wifi, WifiOff } from "lucide-react";
import { useTranslations } from "next-intl";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import {
  LOGS_REFRESH_INTERVAL_DISABLED,
  LOGS_REFRESH_INTERVAL_OPTIONS,
  type LogsRefreshMode,
} from "../_utils/logs-refresh";
import type { UsageLogsStreamStatus } from "../_utils/usage-logs-stream";

interface LogsRefreshControlProps {
  mode: LogsRefreshMode;
  intervalMs: number;
  onModeChange: (mode: LogsRefreshMode) => void;
  onIntervalChange: (intervalMs: number) => void;
  /** 仅推送模式有意义；轮询模式不显示连接状态。 */
  streamStatus: UsageLogsStreamStatus;
  disabled?: boolean;
}

/** 间隔选项的显示名：秒数，或「关闭」（0）。选项是固定表，不随语言变化。 */
function intervalLabel(intervalMs: number): string {
  return intervalMs === LOGS_REFRESH_INTERVAL_DISABLED ? "off" : `${intervalMs / 1000}`;
}

/**
 * 刷新控制：模式（轮询 / 推送）与间隔。
 *
 * 两者都是**本机偏好**（存 localStorage），故在窄屏也保留控件——用户要能自己判断哪种模式
 * 体验更好。推送模式额外显示连接状态：推送是长连接，看不到状态就无从判断「没新数据」还是
 * 「连接断了」。
 */
export function LogsRefreshControl({
  mode,
  intervalMs,
  onModeChange,
  onIntervalChange,
  streamStatus,
  disabled = false,
}: LogsRefreshControlProps) {
  const t = useTranslations("dashboard");
  const isStale = streamStatus === "unauthorized" || streamStatus === "reconnecting";

  return (
    <div className="flex items-center gap-1.5">
      {mode === "push" && (
        <span
          className={cn(
            "inline-flex items-center gap-1 text-[11px]",
            isStale ? "text-destructive" : "text-muted-foreground"
          )}
          title={t(`logs.refresh.streamStatus.${streamStatus}`)}
          aria-label={t(`logs.refresh.streamStatus.${streamStatus}`)}
        >
          {isStale ? <WifiOff className="h-3 w-3" /> : <Wifi className="h-3 w-3" />}
        </span>
      )}

      <Select
        value={mode}
        onValueChange={(value) => onModeChange(value as LogsRefreshMode)}
        disabled={disabled}
      >
        <SelectTrigger
          className="h-8 w-auto gap-1.5 text-xs"
          aria-label={t("logs.refresh.modeLabel")}
        >
          <Radio className="h-3.5 w-3.5" />
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="pull">{t("logs.refresh.modePull")}</SelectItem>
          <SelectItem value="push">{t("logs.refresh.modePush")}</SelectItem>
        </SelectContent>
      </Select>

      <Select
        value={String(intervalMs)}
        onValueChange={(value) => onIntervalChange(Number(value))}
        disabled={disabled}
      >
        <SelectTrigger
          className="h-8 w-auto gap-1.5 text-xs"
          aria-label={t("logs.refresh.intervalLabel")}
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {LOGS_REFRESH_INTERVAL_OPTIONS.map((option) => (
            <SelectItem key={option} value={String(option)}>
              {option === LOGS_REFRESH_INTERVAL_DISABLED
                ? t("logs.refresh.intervalOff")
                : t("logs.refresh.intervalSeconds", { seconds: intervalLabel(option) })}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
