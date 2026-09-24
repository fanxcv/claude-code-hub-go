"use client";

import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, Check, Copy, Loader2, RefreshCw, TrendingUp } from "lucide-react";
import { useLocale, useTranslations } from "next-intl";
import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { getProviderCircuitLogs, getProviderSlowLogs } from "@/lib/api-client/v1/actions/providers";
import type {
  ProviderCircuitLogs,
  ProviderCircuitLogsError,
  ProviderSlowLogEvent,
  ProviderSlowLogs,
} from "@/types/provider";

/**
 * 查看熔断日志：供应商**当前熔断状态** + 该供应商**近期错误**，另有一个 tab 看**低速降权事件**。
 *
 * 两块数据在后端各自独立降级（Redis 读不到只影响状态块、库读不到只影响错误块），故这里也必须
 * 分别渲染各自的「无数据/不可用」——把降级画成一个整体会让人以为整个排障入口坏了。
 * 加 tab 后这条纪律落到**每个 tab 内**：加载/错误态不再放在整弹窗上，否则切到还没取数的
 * 低速 tab 会闪一整页 loading。
 *
 * 为什么最近错误要显示「时间范围」：后端只回看最近 N 小时（默认 24）。不写清这一点，
 * 「24 小时内没有错误」会被读成「从来没有错误」，而这正是最需要避免的误判。
 * 低速 tab 同理（保留 24 小时）。
 */

// 与后端 circuitLogsMaxLimit 一致；这里给一个更小的默认值，首屏要能一眼扫完。
const DEFAULT_LIMIT = 20;

interface ProviderCircuitLogsDialogProps {
  providerId: number;
  providerName: string;
  /**
   * 触发元素。**可选**：桌面端用图标按钮（自带 trigger），移动端要从 DropdownMenuItem
   * 打开（菜单项选中即关菜单，不由 DialogTrigger 控制），那种场景交给父组件传 open/onOpenChange
   * 而不传 trigger。
   */
  trigger?: React.ReactNode;
  /** 受控开关（不给 trigger 时必须传）。 */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
}

export function ProviderCircuitLogsDialog({
  providerId,
  providerName,
  trigger,
  open,
  onOpenChange,
}: ProviderCircuitLogsDialogProps) {
  const t = useTranslations("settings.providers.list.circuitLogs");
  const locale = useLocale();
  const [internalOpen, setInternalOpen] = useState(false);
  const [tab, setTab] = useState<"circuit" | "slow">("circuit");
  const isControlled = open !== undefined;
  const dialogOpen = isControlled ? open : internalOpen;
  const setDialogOpen = isControlled ? (onOpenChange ?? (() => {})) : setInternalOpen;

  // 只在打开时取数：熔断日志是排障用的按需查询，不该在列表页为每个供应商预热。
  const query = useQuery({
    queryKey: ["provider-circuit-logs", providerId, DEFAULT_LIMIT],
    queryFn: () => getProviderCircuitLogs(providerId, DEFAULT_LIMIT),
    enabled: dialogOpen,
    staleTime: 10_000,
  });

  // 低速日志同样是按需查询，且**更懒**一层：切到该 tab 才发请求。
  // 为什么不用 dialogOpen 一个条件：两个请求都在开弹窗时发，会让只来看熔断状态的人
  // 白白多付一次 Redis 读；排障场景按需取数是本弹窗的既有取舍（见上一条注释）。
  const slowQuery = useQuery({
    queryKey: ["provider-slow-logs", providerId, DEFAULT_LIMIT],
    queryFn: () => getProviderSlowLogs(providerId, DEFAULT_LIMIT),
    enabled: dialogOpen && tab === "slow",
    staleTime: 10_000,
  });

  const formatTime = (value: number | null) => {
    if (value === null) return t("valueAbsent");
    return new Date(value).toLocaleString(locale);
  };

  return (
    <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
      {trigger !== undefined ? <DialogTrigger asChild>{trigger}</DialogTrigger> : null}
      <DialogContent className="flex max-h-[85vh] max-w-4xl flex-col overflow-hidden">
        <DialogHeader>
          <DialogTitle>{t("title", { name: providerName })}</DialogTitle>
          <DialogDescription>{t("description")}</DialogDescription>
        </DialogHeader>

        {/* Tabs 承担剩下的高度（min-h-0 是 flex 里能真正收缩的前提），
            每个 TabsContent 自己滚动——原来那一个整弹窗级滚动容器被 tab 结构切开了。 */}
        <Tabs
          value={tab}
          onValueChange={(value) => setTab(value === "slow" ? "slow" : "circuit")}
          className="flex min-h-0 flex-1 flex-col"
        >
          <TabsList>
            <TabsTrigger value="circuit">{t("tabs.circuit")}</TabsTrigger>
            <TabsTrigger value="slow">{t("tabs.slow")}</TabsTrigger>
          </TabsList>

          <TabsContent
            value="circuit"
            className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto pr-1"
          >
            {query.isPending ? (
              <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t("loading")}
              </div>
            ) : query.isError ? (
              <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm">
                <AlertTriangle className="mt-0.5 h-4 w-4 text-destructive" />
                <span>{t("loadFailed")}</span>
              </div>
            ) : (
              <>
                <CircuitStateBlock payload={query.data} formatTime={formatTime} />
                <ErrorsBlock payload={query.data} />
                <div className="flex justify-end">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => query.refetch()}
                    disabled={query.isFetching}
                  >
                    <RefreshCw
                      className={`mr-2 h-4 w-4 ${query.isFetching ? "animate-spin" : ""}`}
                    />
                    {t("refresh")}
                  </Button>
                </div>
              </>
            )}
          </TabsContent>

          <TabsContent
            value="slow"
            className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto pr-1"
          >
            {slowQuery.isPending ? (
              <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" />
                {t("loading")}
              </div>
            ) : slowQuery.isError ? (
              <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm">
                <AlertTriangle className="mt-0.5 h-4 w-4 text-destructive" />
                <span>{t("loadFailed")}</span>
              </div>
            ) : (
              <>
                <SlowLogsBlock payload={slowQuery.data} formatTime={formatTime} />
                <div className="flex justify-end">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => slowQuery.refetch()}
                    disabled={slowQuery.isFetching}
                  >
                    <RefreshCw
                      className={`mr-2 h-4 w-4 ${slowQuery.isFetching ? "animate-spin" : ""}`}
                    />
                    {t("refresh")}
                  </Button>
                </div>
              </>
            )}
          </TabsContent>
        </Tabs>
      </DialogContent>
    </Dialog>
  );
}

/** 熔断状态块：状态、失败计数、开启时刻、恢复倒计时、阈值。 */
function CircuitStateBlock({
  payload,
  formatTime,
}: {
  payload: ProviderCircuitLogs;
  formatTime: (value: number | null) => string;
}) {
  const t = useTranslations("settings.providers.list.circuitLogs");

  if (!payload.circuit.available) {
    return (
      <div className="rounded-md border bg-muted/40 p-3 text-sm">
        <div className="font-medium">{t("state.title")}</div>
        {/* 不用 0 冒充读不到：这里明确说「读不到」，而不是显示一排 0。 */}
        <div className="mt-1 text-muted-foreground">
          {t("state.unavailable", { reason: payload.circuit.unavailableReason ?? "unknown" })}
        </div>
      </div>
    );
  }

  const remaining = payload.circuit.recoveryMinutes;
  const ladderLevel = payload.circuit.consecutiveOpenCount ?? 0;
  const ladderWindow = payload.circuit.openWindowMinutes;
  const ladderChangedAt = payload.circuit.consecutiveOpenCountChangedAt;
  return (
    <div className="rounded-md border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">{t("state.title")}</span>
        <CircuitStateBadge state={payload.circuit.circuitState} />
        {remaining !== null && (
          <span className="text-xs text-muted-foreground">
            {t("state.recoveryMinutes", { minutes: remaining })}
          </span>
        )}
      </div>
      <div className="mt-2 grid grid-cols-2 gap-x-6 gap-y-1 text-xs md:grid-cols-4">
        <Field label={t("state.failureCount")} value={String(payload.circuit.failureCount ?? 0)} />
        <Field
          label={t("state.failureThreshold")}
          value={String(payload.thresholds.failureThreshold)}
        />
        <Field
          label={t("state.lastFailureTime")}
          value={formatTime(payload.circuit.lastFailureTime)}
        />
        <Field
          label={t("state.circuitOpenUntil")}
          value={formatTime(payload.circuit.circuitOpenUntil)}
        />
      </div>

      {/* 等待阶梯：级数 > 0 才展开。**只报「当前值 + 最近一次变化时间」**——
          历史级数序列 Redis 里不存在（哈希只存当前值），回溯不了，
          所以这里不画时间线（那只能是编的）。 */}
      {ladderLevel > 0 && (
        <div className="mt-3 rounded border border-sky-200 bg-sky-50/60 p-2 dark:border-sky-800 dark:bg-sky-950/30">
          <div className="flex items-center gap-1.5 text-xs font-medium text-sky-800 dark:text-sky-300">
            <TrendingUp className="h-3.5 w-3.5" />
            {t("ladder.title")}
          </div>
          <div className="mt-1.5 grid grid-cols-2 gap-x-6 gap-y-1 text-xs md:grid-cols-3">
            <Field label={t("ladder.level")} value={String(ladderLevel)} />
            <Field
              label={t("ladder.window")}
              value={
                ladderWindow !== null ? t("ladder.windowMinutes", { minutes: ladderWindow }) : "-"
              }
            />
            <Field label={t("ladder.changedAt")} value={formatTime(ladderChangedAt)} />
          </div>
          <p className="mt-1.5 text-[11px] leading-relaxed text-muted-foreground">
            {t("ladder.explanation")}
          </p>
        </div>
      )}
    </div>
  );
}

/** 错误列表块：时间范围 + 逐条错误。 */
function ErrorsBlock({ payload }: { payload: ProviderCircuitLogs }) {
  const t = useTranslations("settings.providers.list.circuitLogs");

  if (payload.errorsUnavailableReason !== null) {
    return (
      <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm dark:border-amber-700 dark:bg-amber-950/30">
        {t("errors.unavailable", { reason: payload.errorsUnavailableReason })}
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="text-sm font-medium">{t("errors.title")}</span>
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">
            {t("errors.window", {
              hours: payload.window.lookbackHours,
              limit: payload.window.limit,
              since: new Date(payload.window.since).toLocaleString(),
            })}
          </span>
          {/* 一键拷走全部（按表格列顺序拼成纯文本），便于贴进工单/聊天。 */}
          <CopyButton
            text={formatErrorsForClipboard(payload)}
            label={t("errors.copyAll")}
            copiedLabel={t("errors.copied")}
            failedLabel={t("errors.copyFailed")}
            variant="outline"
          />
        </div>
      </div>
      {payload.errors.length === 0 ? (
        <div className="rounded-md border bg-muted/40 p-3 text-sm text-muted-foreground">
          {t("errors.empty", { hours: payload.window.lookbackHours })}
        </div>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="whitespace-nowrap">{t("errors.columns.time")}</TableHead>
                <TableHead className="whitespace-nowrap">{t("errors.columns.model")}</TableHead>
                <TableHead className="whitespace-nowrap">{t("errors.columns.status")}</TableHead>
                <TableHead className="whitespace-nowrap">{t("errors.columns.source")}</TableHead>
                <TableHead>{t("errors.columns.message")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {payload.errors.map((row) => (
                <ErrorRow key={row.requestId} row={row} />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

function ErrorRow({ row }: { row: ProviderCircuitLogsError }) {
  const t = useTranslations("settings.providers.list.circuitLogs");
  // createdAt 是 ISO 串；转成本地时刻显示，与界面其它时间列同口径。
  const createdAt = Number.isNaN(Date.parse(row.createdAt))
    ? row.createdAt
    : new Date(row.createdAt).toLocaleString();

  return (
    <TableRow>
      <TableCell className="whitespace-nowrap text-xs">
        {createdAt}
        {row.durationMs !== null && (
          <span className="ml-1 text-muted-foreground">
            ({t("errors.durationMs", { ms: row.durationMs })})
          </span>
        )}
      </TableCell>
      <TableCell className="max-w-[10rem] truncate text-xs" title={row.model ?? ""}>
        {row.model ?? t("valueAbsent")}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs">
        {row.statusCode === null ? (
          // 空状态码是**有意义**的：「没有发生过 HTTP 交换」（本地拒绝/客户端中断）。
          // 显示成 0 会让人以为上游回了 0，故用文字说明。
          <span className="text-muted-foreground">{t("errors.noStatusCode")}</span>
        ) : (
          <Badge variant={row.statusCode >= 500 ? "destructive" : "outline"}>
            {row.statusCode}
          </Badge>
        )}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs">
        {row.source === "chain" ? (
          <span title={row.chainReason ?? ""}>
            {t("errors.sourceChain")}
            {row.chainReason ? ` · ${row.chainReason}` : ""}
          </span>
        ) : (
          t("errors.sourceDirect")
        )}
      </TableCell>
      <TableCell className="min-w-[20rem] max-w-[46rem] text-xs">
        {/* whitespace-pre-wrap：上游错误常是多行（含 JSON / 栈），默认折叠换行会把它挤成一行
            而看起来「不完整」；break-words 而非 break-all：长 URL/请求 id 仍能折行，但
            普通英文单词不被拦腰截断。 */}
        <div className="whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed">
          {row.errorMessage ?? t("valueAbsent")}
        </div>
        <div className="mt-1 flex items-center gap-2">
          <CopyButton
            text={row.errorMessage ?? ""}
            label={t("errors.copy")}
            copiedLabel={t("errors.copied")}
            failedLabel={t("errors.copyFailed")}
          />
          {row.redacted && (
            // 让运维知道「这是被改写过的文案」——否则会拿着 [REDACTED_KEY] 去搜上游日志而搜不到。
            <span className="text-[11px] text-muted-foreground">{t("errors.redactedHint")}</span>
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

/** 低速日志块：时间范围 + 逐条降权/基线事件。 */
function SlowLogsBlock({
  payload,
  formatTime,
}: {
  payload: ProviderSlowLogs;
  formatTime: (value: number | null) => string;
}) {
  const t = useTranslations("settings.providers.list.circuitLogs");

  if (payload.unavailableReason !== null) {
    return (
      <div className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm dark:border-amber-700 dark:bg-amber-950/30">
        {t("slow.unavailable", { reason: payload.unavailableReason })}
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="text-sm font-medium">{t("slow.title")}</span>
        {/* 时间范围必须显示（同 errors.window 的纪律）：否则「24h 内无降权」会被读成「从未降权」。 */}
        <span className="text-xs text-muted-foreground">
          {t("slow.window", {
            hours: payload.window.retentionHours,
            limit: payload.window.limit,
            since: new Date(payload.window.since).toLocaleString(),
          })}
        </span>
      </div>
      {/* 隔离运行态（当前档位）先于改道汇总（历史后果）：运维最先要看的是「现在挡不挡」。 */}
      <QuarantineBlock payload={payload} />
      {/* 改道汇总（用户 2026-09-22 需求）：回答「压下来之后实际挡掉了多少流量」。
          与下面的事件表分工——事件回答「什么时候被压/恢复」，本行回答「后果有多大」。

          用真值判断而不是 `!== null`：后端「未装配」时为 null，而**更旧的响应**可能根本没这个键
          （undefined）。两者都不该显示这一行，`!== null` 会把 undefined 放过去、当场崩掉整块。 */}
      {payload.diverts ? (
        <div className="rounded-md border bg-muted/40 p-3 text-sm">
          {t("slow.diverts.summary", {
            hours: payload.diverts.windowHours,
            total: payload.diverts.total,
            cooldown: payload.diverts.cooldown,
            penalty: payload.diverts.penalty,
          })}
        </div>
      ) : null}
      {payload.events.length === 0 ? (
        <div className="rounded-md border bg-muted/40 p-3 text-sm text-muted-foreground">
          {t("slow.empty", { hours: payload.window.retentionHours })}
        </div>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="whitespace-nowrap">{t("slow.columns.time")}</TableHead>
                <TableHead className="whitespace-nowrap">{t("slow.columns.kind")}</TableHead>
                <TableHead className="whitespace-nowrap">{t("slow.columns.model")}</TableHead>
                <TableHead>{t("slow.columns.detail")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {payload.events.map((event, index) => (
                <SlowLogRow
                  // 存储层事件没有唯一 id（同毫秒可并存多条）；用索引做键，
                  // 列表是只读且不重排的，索引键在这是安全的。
                  key={`${event.at}-${event.kind}-${index}`}
                  event={event}
                  formatTime={formatTime}
                />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}

function SlowLogRow({
  event,
  formatTime,
}: {
  event: ProviderSlowLogEvent;
  formatTime: (value: number | null) => string;
}) {
  const t = useTranslations("settings.providers.list.circuitLogs");
  return (
    <TableRow>
      <TableCell className="whitespace-nowrap text-xs">{formatTime(event.at)}</TableCell>
      <TableCell className="whitespace-nowrap text-xs">{t(`slow.kinds.${event.kind}`)}</TableCell>
      <TableCell className="whitespace-nowrap text-xs">{event.modelKey ?? "-"}</TableCell>
      <TableCell className="text-xs">{formatSlowLogDetail(event, t)}</TableCell>
    </TableRow>
  );
}

/**
 * 事件的「详情」列：按种类渲染各自有意义的数字。
 *
 * 用 null 判断而不是 `?? 0`：后端用 null 表达「本事件不含此维」，渲染成 0 会说谎
 * （例如把基线事件的降权量显示成「0 → 0」）。
 */
function formatSlowLogDetail(
  event: ProviderSlowLogEvent,
  t: ReturnType<typeof useTranslations>
): string {
  if (event.kind === "penalty_up" || event.kind === "penalty_down") {
    // 渲染 reason 才能区分「真恢复解除」（reason=recovery）与「滑窗衰减重算」——
    // 两者都是 penalty_down，只看 from/to 在界面上同形（真恢复 from=N to=0，重算也可能 to=0）。
    const from = event.penaltyFrom ?? 0;
    const to = event.penaltyTo ?? 0;
    return event.reason
      ? t("slow.detail.penaltyReason", { from, to, reason: event.reason })
      : t("slow.detail.penalty", { from, to });
  }
  if (event.kind === "quarantine_entered") {
    return t("slow.detail.quarantine", { reason: event.reason ?? "-" });
  }
  if (event.kind === "baseline_published") {
    return t("slow.detail.baseline", {
      median: event.median ?? 0,
      samples: event.samples ?? 0,
      source: event.reason ?? "-",
    });
  }
  return t("slow.detail.revoked", { reason: event.reason ?? "-" });
}

/**
 * 隔离运行态块：回答「机制现在在哪一档」，与事件/改道读数（历史）分工。
 *
 * 为何必须有它：只有改道读数时，一个正在隔离的渠道若恰好改道数为 0，界面会显示「因低速被改道 0 次」
 * 这个伪零，让人误判「没在挡流量」。本块直接给出每组合的隔离标记与放行比例。
 */
function QuarantineBlock({ payload }: { payload: ProviderSlowLogs }) {
  const t = useTranslations("settings.providers.list.circuitLogs");
  // null 表示后端未装配该读面（与「无组合处于隔离」不同），不渲染这一块。
  if (!payload.quarantine) return null;
  // 事件流保留窗内的进入隔离次数：一次隔离期一条（见后端 slowlog），故即「窗内隔离多少次」。
  const enteredCount = payload.events.filter((event) => event.kind === "quarantine_entered").length;
  return (
    <div className="rounded-md border bg-muted/40 p-3 text-sm">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="font-medium">{t("slow.quarantine.title")}</span>
        <span className="text-xs text-muted-foreground">
          {t("slow.quarantine.enteredCount", {
            hours: payload.window.retentionHours,
            count: enteredCount,
          })}
        </span>
      </div>
      {payload.quarantine.unavailableReason !== null ? (
        <div className="mt-1 text-muted-foreground">
          {t("slow.quarantine.unavailable", { reason: payload.quarantine.unavailableReason })}
        </div>
      ) : payload.quarantine.combinations.length === 0 ? (
        <div className="mt-1 text-muted-foreground">{t("slow.quarantine.empty")}</div>
      ) : (
        <div className="mt-2 space-y-1">
          {payload.quarantine.combinations.map((combination) => (
            <div
              key={combination.modelKey}
              className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs"
            >
              <span className="font-mono">{combination.modelKey}</span>
              <Badge variant={combination.quarantined ? "destructive" : "outline"}>
                {combination.quarantined
                  ? t("slow.quarantine.stateQuarantined")
                  : t("slow.quarantine.stateNormal")}
              </Badge>
              <span>{t("slow.quarantine.penalty", { value: combination.penalty })}</span>
              <span>
                {t("slow.quarantine.admission", { permille: combination.admissionPermille })}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/**
 * 复制到剪贴板：优先 Clipboard API；非安全上下文（http / 权限被拦）退回 textarea + execCommand。
 * 返回是否成功，供按钮显式区分「已复制」与「复制失败」——静默失败会让用户以为拷到了。
 */
async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // 落到下方兜底路径
  }
  try {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.top = "-1000px";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(area);
    return ok;
  } catch {
    return false;
  }
}

/**
 * 把错误列表拼成纯文本（表头 + 每条一行，多行错误整体保留）。
 * 复制给的是**未经界面截断的全文**，与屏幕上看到的一致。
 */
function formatErrorsForClipboard(payload: ProviderCircuitLogs): string {
  const header = ["time", "model", "status", "source", "message"].join("\t");
  const lines = payload.errors.map((row) => {
    const source =
      row.source === "chain" ? `chain${row.chainReason ? `:${row.chainReason}` : ""}` : "direct";
    return [
      row.createdAt,
      row.model ?? "-",
      row.statusCode ?? "-",
      source,
      row.errorMessage ?? "-",
    ].join("\t");
  });
  return [header, ...lines].join("\n");
}

/** 复制按钮：点后短暂显示「已复制」/「复制失败」，避免静默成功或静默失败。 */
function CopyButton({
  text,
  label,
  copiedLabel,
  failedLabel,
  variant = "ghost",
}: {
  text: string;
  label: string;
  copiedLabel: string;
  failedLabel: string;
  variant?: "ghost" | "outline";
}) {
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");

  const onClick = async () => {
    const ok = await copyToClipboard(text);
    setState(ok ? "ok" : "fail");
    window.setTimeout(() => setState("idle"), 1600);
  };

  return (
    <Button
      type="button"
      size="sm"
      variant={variant}
      className="h-6 gap-1 px-2 text-[11px]"
      onClick={(e) => {
        e.stopPropagation();
        void onClick();
      }}
      title={label}
    >
      {state === "ok" ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
      {state === "ok" ? copiedLabel : state === "fail" ? failedLabel : label}
    </Button>
  );
}

/** 熔断状态徽标：三态各一色，与列表页 existing 徽标语义一致。 */
function CircuitStateBadge({ state }: { state: "closed" | "open" | "half-open" | null }) {
  const t = useTranslations("settings.providers.list.circuitLogs");
  if (state === "open") {
    return (
      <Badge variant="destructive" className="flex items-center gap-1">
        <AlertTriangle className="h-3 w-3" />
        {t("state.open")}
      </Badge>
    );
  }
  if (state === "half-open") {
    return (
      <Badge
        variant="outline"
        className="border-amber-300 bg-amber-100 text-amber-700 dark:border-amber-700 dark:bg-amber-900/30 dark:text-amber-400"
      >
        {t("state.halfOpen")}
      </Badge>
    );
  }
  return <Badge variant="outline">{t("state.closed")}</Badge>;
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-muted-foreground">{label}</div>
      <div className="font-medium">{value}</div>
    </div>
  );
}
