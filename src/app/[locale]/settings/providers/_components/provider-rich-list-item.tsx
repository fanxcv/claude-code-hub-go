"use client";
import { VisuallyHidden } from "@radix-ui/react-visually-hidden";
import { useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  AlertTriangle,
  ArrowRightLeft,
  CheckCircle,
  Clock,
  Copy,
  Edit,
  Globe,
  Key,
  MoreHorizontal,
  RotateCcw,
  ScrollText,
  ShieldCheck,
  Trash,
  TrendingDown,
  TrendingUp,
  XCircle,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { memo, useCallback, useEffect, useState, useTransition } from "react";
import { toast } from "sonner";
import { FormErrorBoundary } from "@/components/form-error-boundary";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  editProvider,
  getUnmaskedProviderKey,
  removeProvider,
  resetProviderCircuit,
  resetProviderTotalUsage,
  undoProviderDelete,
} from "@/lib/api-client/v1/actions/providers";
import {
  PROVIDER_GROUP,
  PROVIDER_LIMITS,
  PROVIDER_TIMEOUT_DEFAULTS,
} from "@/lib/constants/provider.constants";
import { PROVIDER_BATCH_PATCH_ERROR_CODES } from "@/lib/provider-batch-patch-error-codes";
import { getProviderTypeConfig, getProviderTypeTranslationKey } from "@/lib/provider-type-utils";
import { cn } from "@/lib/utils";
import { copyToClipboard, isClipboardSupported } from "@/lib/utils/clipboard";
import { getContrastTextColor, getGroupColor } from "@/lib/utils/color";
import type { CurrencyCode } from "@/lib/utils/currency";
import { formatCurrency } from "@/lib/utils/currency";
import { normalizeProviderGroupTag, parseProviderGroups } from "@/lib/utils/provider-group";
import type {
  ProviderCircuitHealth,
  ProviderDisplay,
  ProviderStatistics,
  ProviderVendor,
} from "@/types/provider";
import { ProviderForm } from "./forms/provider-form";
import { GroupEditCombobox } from "./group-edit-combobox";
import { InlineEditPopover } from "./inline-edit-popover";
import { invalidateProviderQueries } from "./invalidate-provider-queries";
import { PriorityEditPopover } from "./priority-edit-popover";
import { ProviderCircuitLogsDialog } from "./provider-circuit-logs-dialog";
import { ProviderEndpointHover } from "./provider-endpoint-hover";
import { ProviderFormDialogContent } from "./provider-form-dialog-content";
import type { ProviderViewer } from "./provider-viewer";

interface ProviderRichListItemProps {
  provider: ProviderDisplay;
  vendor?: ProviderVendor;
  currentUser?: ProviderViewer;
  healthStatus?: ProviderCircuitHealth;
  /** Endpoint-level circuit breaker info for this provider */
  endpointCircuitInfo?: Array<{
    endpointId: number;
    circuitState: "closed" | "open" | "half-open";
    failureCount: number;
    circuitOpenUntil: number | null;
  }>;
  statistics?: ProviderStatistics;
  statisticsLoading?: boolean;
  currencyCode?: CurrencyCode;
  enableMultiProviderTypes: boolean;
  isMultiSelectMode?: boolean;
  isSelected?: boolean;
  activeGroupFilter?: string | null;
  onSelectChange?: (checked: boolean) => void;
  onEdit?: () => void;
  onClone?: () => void;
  onDelete?: () => void;
  allGroups?: string[];
  userGroups?: string[];
  isAdmin?: boolean;
}

function ProviderRichListItemInner({
  provider,
  vendor,
  currentUser,
  healthStatus,
  endpointCircuitInfo = [],
  statistics,
  statisticsLoading = false,
  currencyCode = "USD",
  enableMultiProviderTypes,
  isMultiSelectMode = false,
  isSelected = false,
  activeGroupFilter = null,
  onSelectChange,
  onEdit: onEditProp,
  onClone: onCloneProp,
  onDelete: onDeleteProp,
  allGroups = [],
  userGroups = [],
  isAdmin = false,
}: ProviderRichListItemProps) {
  const queryClient = useQueryClient();

  const doInvalidate = useCallback(() => invalidateProviderQueries(queryClient), [queryClient]);

  const [openEdit, setOpenEdit] = useState(false);
  const [openClone, setOpenClone] = useState(false);
  const [showKeyDialog, setShowKeyDialog] = useState(false);

  // Defer heavy ProviderForm mount so dialog animation doesn't compete with React work
  const [editFormReady, setEditFormReady] = useState(false);
  const [cloneFormReady, setCloneFormReady] = useState(false);

  useEffect(() => {
    if (openEdit) {
      let cancelled = false;
      const id = requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          if (!cancelled) setEditFormReady(true);
        });
      });
      return () => {
        cancelled = true;
        cancelAnimationFrame(id);
      };
    }
    setEditFormReady(false);
  }, [openEdit]);

  useEffect(() => {
    if (openClone) {
      let cancelled = false;
      const id = requestAnimationFrame(() => {
        requestAnimationFrame(() => {
          if (!cancelled) setCloneFormReady(true);
        });
      });
      return () => {
        cancelled = true;
        cancelAnimationFrame(id);
      };
    }
    setCloneFormReady(false);
  }, [openClone]);
  const [mobileDeleteDialogOpen, setMobileDeleteDialogOpen] = useState(false);
  // 熔断日志：移动端从菜单项打开（菜单选中即关，故由父层受控），桌面端用图标按钮自触发。
  const [circuitLogsOpen, setCircuitLogsOpen] = useState(false);
  const [unmaskedKey, setUnmaskedKey] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [clipboardAvailable, setClipboardAvailable] = useState(false);
  const [resetPending, startResetTransition] = useTransition();
  const [resetUsagePending, startResetUsageTransition] = useTransition();
  const [deletePending, startDeleteTransition] = useTransition();
  const [togglePending, startToggleTransition] = useTransition();

  const canEdit = currentUser?.role === "admin";
  const t = useTranslations("settings.providers");
  const tTypes = useTranslations("settings.providers.types");
  const tList = useTranslations("settings.providers.list");
  const tBatchEdit = useTranslations("settings.providers.batchEdit");
  const tTimeout = useTranslations("settings.providers.form.sections.timeout");
  const tInline = useTranslations("settings.providers.inlineEdit");

  const validatePriority = (raw: string) => {
    if (raw.length === 0) return tInline("priorityInvalid");
    const value = Number(raw);
    if (!Number.isFinite(value) || !Number.isInteger(value) || value < 0 || value > 2147483647)
      return tInline("priorityInvalid");
    return null;
  };

  const validateWeight = (raw: string) => {
    if (raw.length === 0) return tInline("weightInvalid");
    const value = Number(raw);
    if (
      !Number.isFinite(value) ||
      !Number.isInteger(value) ||
      value < PROVIDER_LIMITS.WEIGHT.MIN ||
      value > PROVIDER_LIMITS.WEIGHT.MAX
    )
      return tInline("weightInvalid");
    return null;
  };

  const validateCostMultiplier = (raw: string) => {
    if (raw.length === 0) return tInline("costMultiplierInvalid");
    const value = Number(raw);
    if (!Number.isFinite(value) || value < 0) return tInline("costMultiplierInvalid");
    return null;
  };

  // 获取供应商类型配置
  const typeConfig = getProviderTypeConfig(provider.providerType);
  const TypeIcon = typeConfig.icon;
  const typeKey = getProviderTypeTranslationKey(provider.providerType);
  const typeLabel = tTypes(`${typeKey}.label`);
  const typeDescription = tTypes(`${typeKey}.description`);

  useEffect(() => {
    setClipboardAvailable(isClipboardSupported());
  }, []);

  // 处理编辑
  const handleEdit = () => {
    if (onEditProp) {
      onEditProp();
    } else {
      setOpenEdit(true);
    }
  };

  // 处理克隆
  const handleClone = () => {
    if (onCloneProp) {
      onCloneProp();
    } else {
      setOpenClone(true);
    }
  };

  // 处理删除
  const handleDelete = () => {
    if (onDeleteProp) {
      onDeleteProp();
    } else {
      startDeleteTransition(async () => {
        try {
          const res = await removeProvider(provider.id);
          if (res.ok) {
            const undoToken = res.data.undoToken;
            const operationId = res.data.operationId;

            toast.success(tBatchEdit("undo.singleDeleteSuccess"), {
              duration: 10000,
              action: {
                label: tBatchEdit("undo.button"),
                onClick: async () => {
                  try {
                    const undoResult = await undoProviderDelete({ undoToken, operationId });
                    if (undoResult.ok) {
                      toast.success(tBatchEdit("undo.singleDeleteUndone"));
                      await doInvalidate();
                    } else if (
                      undoResult.errorCode === PROVIDER_BATCH_PATCH_ERROR_CODES.UNDO_EXPIRED
                    ) {
                      toast.error(tBatchEdit("undo.expired"));
                    } else {
                      toast.error(tBatchEdit("undo.failed"));
                    }
                  } catch {
                    toast.error(tBatchEdit("undo.failed"));
                  }
                },
              },
            });

            doInvalidate();
          } else {
            toast.error(tList("deleteFailed"), {
              description: res.error || tList("unknownError"),
            });
          }
        } catch (error) {
          console.error("Failed to delete provider:", error);
          toast.error(tList("deleteFailed"), {
            description: tList("deleteError"),
          });
        }
      });
    }
  };

  // 处理查看密钥
  const handleShowKey = async () => {
    setShowKeyDialog(true);
    try {
      const result = await getUnmaskedProviderKey(provider.id);
      if (result.ok) {
        setUnmaskedKey(result.data.key);
      } else {
        toast.error(tList("getKeyFailed"), {
          description: result.error || tList("unknownError"),
        });
        setShowKeyDialog(false);
      }
    } catch (error) {
      console.error("Failed to get provider key:", error);
      toast.error(tList("getKeyFailed"), {
        description: tList("unknownError"),
      });
      setShowKeyDialog(false);
    }
  };

  // 处理复制密钥
  const handleCopy = async () => {
    if (unmaskedKey) {
      const success = await copyToClipboard(unmaskedKey);

      if (success) {
        setCopied(true);
        toast.success(tList("keyCopied"));
        setTimeout(() => setCopied(false), 3000);
      } else {
        toast.error(tList("copyFailed"));
      }
    }
  };

  // 处理关闭 Dialog
  const handleCloseDialog = () => {
    setShowKeyDialog(false);
    setUnmaskedKey(null);
    setCopied(false);
  };

  // 处理手动解除熔断
  const handleResetCircuit = () => {
    startResetTransition(async () => {
      try {
        const res = await resetProviderCircuit(provider.id);
        if (res.ok) {
          toast.success(tList("resetCircuitSuccess"), {
            description: tList("resetCircuitSuccessDesc", { name: provider.name }),
          });
          doInvalidate();
        } else {
          toast.error(tList("resetCircuitFailed"), {
            description: res.error || tList("unknownError"),
          });
        }
      } catch (error) {
        console.error("Failed to reset circuit breaker:", error);
        toast.error(tList("resetCircuitFailed"), {
          description: tList("deleteError"),
        });
      }
    });
  };

  // 处理手动重置总用量（总限额用）
  const handleResetTotalUsage = () => {
    startResetUsageTransition(async () => {
      try {
        const res = await resetProviderTotalUsage(provider.id);
        if (res.ok) {
          toast.success(tList("resetUsageSuccess"), {
            description: tList("resetUsageSuccessDesc", { name: provider.name }),
          });
          doInvalidate();
        } else {
          toast.error(tList("resetUsageFailed"), {
            description: res.error || tList("unknownError"),
          });
        }
      } catch (error) {
        console.error("Failed to reset total usage:", error);
        toast.error(tList("resetUsageFailed"), {
          description: tList("deleteError"),
        });
      }
    });
  };

  // 处理启用/禁用切换
  const handleToggle = () => {
    startToggleTransition(async () => {
      try {
        const res = await editProvider(provider.id, {
          is_enabled: !provider.isEnabled,
        });
        if (res.ok) {
          const status = !provider.isEnabled ? tList("statusEnabled") : tList("statusDisabled");
          toast.success(tList("toggleSuccess", { status }), {
            description: tList("toggleSuccessDesc", { name: provider.name }),
          });
          doInvalidate();
        } else {
          toast.error(tList("toggleFailed"), {
            description: res.error || tList("unknownError"),
          });
        }
      } catch (error) {
        console.error("Failed to toggle provider status:", error);
        toast.error(tList("toggleFailed"), {
          description: tList("deleteError"),
        });
      }
    });
  };

  const createSaveHandler = (fieldName: "priority" | "weight" | "cost_multiplier") => {
    return async (value: number) => {
      try {
        const res = await editProvider(provider.id, { [fieldName]: value } as Parameters<
          typeof editProvider
        >[1]);
        if (res.ok) {
          toast.success(tInline("saveSuccess"));
          doInvalidate();
          return true;
        }
        toast.error(tInline("saveFailed"), { description: res.error || tList("unknownError") });
        return false;
      } catch (error) {
        console.error(`Failed to update ${fieldName}:`, error);
        toast.error(tInline("saveFailed"), { description: tList("unknownError") });
        return false;
      }
    };
  };

  const handleSaveWeight = createSaveHandler("weight");
  const handleSaveCostMultiplier = createSaveHandler("cost_multiplier");

  const providerGroups = parseProviderGroups(provider.groupTag);

  const handleSaveGroups = async (groups: string[]): Promise<boolean> => {
    try {
      const groupTag = normalizeProviderGroupTag(groups.join(","));
      const res = await editProvider(provider.id, { group_tag: groupTag });
      if (res.ok) {
        toast.success(tInline("saveSuccess"));
        queryClient.invalidateQueries({ queryKey: ["providers"] });
        return true;
      }
      toast.error(tInline("groupSaveError"), {
        description: res.error || tList("unknownError"),
      });
      return false;
    } catch (error) {
      console.error("Failed to save groups:", error);
      toast.error(tInline("groupSaveError"), { description: tList("unknownError") });
      return false;
    }
  };

  const handleSavePriorityWithGroups = async (
    newGlobal: number,
    newGroupPriorities: Record<string, number> | null
  ): Promise<boolean> => {
    try {
      const res = await editProvider(provider.id, {
        priority: newGlobal,
        group_priorities: newGroupPriorities,
      });
      if (res.ok) {
        toast.success(tInline("saveSuccess"));
        queryClient.invalidateQueries({ queryKey: ["providers"] });
        return true;
      }
      toast.error(tInline("saveFailed"), { description: res.error || tList("unknownError") });
      return false;
    } catch (error) {
      console.error("Failed to update priority:", error);
      toast.error(tInline("saveFailed"), { description: tList("unknownError") });
      return false;
    }
  };

  const hasKeyCircuitOpen = healthStatus?.circuitState === "open";
  const hasEndpointCircuitOpen = endpointCircuitInfo?.some((ep) => ep.circuitState === "open");

  // 等待阶梯的级数：**只在 n > 0 时**呈现。
  //
  // 为何 n = 0 不显示：首次开闸的窗口就是熔断时长（与未启用阶梯逐字段一致），
  // 而本页绝大多数供应商都不会配阶梯，给它们都堆一个「第 0 阶 / 窗口 30 分钟」
  // 只是噪声。窗口时长同理（服务端在 n = 0 时就不报这个数）。
  const ladderLevel = healthStatus?.consecutiveOpenCount ?? 0;
  const ladderWindowMinutes = healthStatus?.openWindowMinutes ?? null;

  // 熔断徽标（含阶梯徽标）只在**启用**的供应商上呈现。
  //
  // 为什么：熔断读数存在 Redis（TTL 24h），供应商被禁用后旧状态照样留着，而它**不参与选路**
  // （选路只从启用的供应商里挑）。于是界面上会出现「已禁用 + 熔断恢复中」这种自相矛盾的组合，
  // 管理员据此去排查熔断，实际原因却是「它被禁用了」——这是用户实报的误导。
  // 处置放在展示层而非接口层：接口照实回 Redis 读数（口径不变），仅把「与选路无关的读数」从行上收起。
  // 禁用状态本身另有明确标示（行上的启用开关勾选态 + 左侧灰色边条），信息不会丢。
  const showCircuitBadges = provider.isEnabled;
  // 阶梯徽标跟同一开关：它是「熔断的细节」，给禁用者报级数同样是误导。
  const showLadderBadge = showCircuitBadges && ladderLevel > 0;

  // 低速降权：只在有生效降权时呈现（`penalty` 为 null 即「无降权」或「读不到」，两者都不占位）。
  //
  // 与熔断徽标同一条纪律（上面 showCircuitBadges 的说明）：读数存在 Redis，停用渠道的
  // 旧降权照样留着，而它**不参与选路**——给禁用渠道堆降权徽标会引导管理员去查一件不存在的
  // 问题。禁用本身另有明确标示（开关勾选态 + 左侧灰边条），信息不会丢。
  //
  // 读不到（available=false）同样不占位：本徽标回答「被压了多少」，读不到时无值可报
  // （而「读不到」另有熔断面已建立的那套语义，不在这里编一个 0）。
  const slowRatePenalty = healthStatus?.slowRate?.penalty ?? null;
  const showSlowRateBadge = showCircuitBadges && slowRatePenalty !== null;
  const slowRateModelKey = healthStatus?.slowRate?.modelKey ?? null;
  const slowRateCombinations = healthStatus?.slowRate?.combinations ?? 0;

  // 实时并发：只在统计开启且读得到时呈现。
  //
  // 三态各自有原因不占位：
  //   - trackingEnabled 为假：统计根本没在跑（服务端也不会给数）；
  //   - available 为假：开着但读不到（无值可报，不在这里编一个 0）；
  //   - activeSessions 为 null：同上（与 available 同源的双保险）。
  // 与 slowRate 徽标同一条纪律（showCircuitBadges）：停用渠道不报读数。
  const concurrency = healthStatus?.concurrency ?? null;
  const concurrencyActive =
    concurrency?.trackingEnabled === true &&
    concurrency.available &&
    concurrency.activeSessions !== null
      ? concurrency.activeSessions
      : null;
  const showConcurrencyBadge = showCircuitBadges && concurrencyActive !== null;
  // 并发上限：渠道级配置，为 0（未设）时只显示分子，不显示一个假的「/0」。
  const concurrencyLimit = provider.limitConcurrentSessions ?? 0;
  const accentColor = hasKeyCircuitOpen
    ? "border-l-red-500"
    : hasEndpointCircuitOpen
      ? "border-l-amber-500"
      : provider.isEnabled
        ? "border-l-emerald-500"
        : "border-l-gray-300 dark:border-l-gray-600";

  return (
    <>
      <div
        className={cn(
          "rounded-lg border border-l-[3px] bg-card shadow-sm p-4",
          "md:shadow-none md:rounded-none md:border-0 md:border-b md:border-l-[3px] md:bg-transparent md:p-0 md:py-3 md:px-4",
          "flex flex-col gap-3 md:flex-row md:items-center md:gap-4",
          "hover:bg-muted/50 transition-colors",
          accentColor
        )}
      >
        {/* Checkbox: shared between mobile and desktop */}
        {isMultiSelectMode && (
          <Checkbox
            checked={isSelected}
            onCheckedChange={(checked) => onSelectChange?.(Boolean(checked))}
            onClick={(e) => e.stopPropagation()}
            aria-label={tList("selectProvider", { name: provider.name })}
            className="flex-shrink-0"
          />
        )}

        {/* Mobile: top row with name and switch */}
        <div className="flex items-center justify-between md:hidden">
          <div className="flex items-center gap-2 min-w-0 flex-1">
            {provider.isEnabled ? (
              <CheckCircle className="h-4 w-4 text-green-500 flex-shrink-0" />
            ) : (
              <XCircle className="h-4 w-4 text-gray-400 flex-shrink-0" />
            )}
            <div
              className={`flex items-center justify-center w-6 h-6 rounded ${typeConfig.bgColor} flex-shrink-0`}
              title={`${typeLabel} - ${typeDescription}`}
              aria-label={typeLabel}
            >
              <TypeIcon className="h-3.5 w-3.5" aria-hidden />
            </div>
            <span className="font-semibold truncate">{provider.name}</span>
          </div>
          {canEdit && (
            <Switch
              aria-label={provider.name}
              checked={provider.isEnabled}
              onCheckedChange={handleToggle}
              disabled={togglePending}
              className="data-[state=checked]:bg-green-500"
            />
          )}
        </div>

        {/* Mobile: status badges */}
        <div className="flex flex-wrap items-center gap-1.5 md:hidden">
          {canEdit ? (
            <GroupEditCombobox
              currentGroups={providerGroups}
              allGroups={allGroups}
              userGroups={userGroups}
              isAdmin={isAdmin}
              onSave={handleSaveGroups}
            />
          ) : providerGroups.length > 0 ? (
            providerGroups.map((tag, index) => {
              const bgColor = getGroupColor(tag);
              return (
                <Badge
                  key={`${tag}-${index}`}
                  className="text-xs"
                  style={{ backgroundColor: bgColor, color: getContrastTextColor(bgColor) }}
                >
                  {tag}
                </Badge>
              );
            })
          ) : (
            <Badge variant="outline">{PROVIDER_GROUP.DEFAULT}</Badge>
          )}
          {/* Key-level circuit badge */}
          {showCircuitBadges && healthStatus?.circuitState === "open" && (
            <Badge variant="destructive" className="flex items-center gap-1">
              <AlertTriangle className="h-3 w-3" />
              {tList("keyCircuitBroken")}
            </Badge>
          )}
          {/* Key-level half-open badge：熔断窗口已过期、正放行试探。
              与「熔断」分开呈现：那一态会真的拦下请求，半开是放行的（与数据面 ProviderOpen
              同源判定）；把半开报成「熔断」就是生产上看到的那个分叉。 */}
          {showCircuitBadges && healthStatus?.circuitState === "half-open" && (
            <Badge
              variant="outline"
              className="flex items-center gap-1 bg-amber-100 text-amber-700 border-amber-300 hover:bg-amber-200 dark:bg-amber-900/30 dark:text-amber-400 dark:border-amber-700"
            >
              <AlertTriangle className="h-3 w-3" />
              {tList("keyCircuitHalfOpen")}
            </Badge>
          )}
          {/* 等待阶梯：只在 n > 0 时出现，且把「第几阶 / 本次窗口多长」一并说清。
              单看一个「熔断恢复中」分不出“它是不是一直在失败”——这一眼就能看出。 */}
          {showLadderBadge && (
            <Badge
              variant="outline"
              className="flex items-center gap-1 bg-sky-100 text-sky-700 border-sky-300 hover:bg-sky-200 dark:bg-sky-900/30 dark:text-sky-400 dark:border-sky-700"
              title={tList("ladder.tooltip")}
            >
              <TrendingUp className="h-3 w-3" />
              {ladderWindowMinutes !== null
                ? tList("ladder.badgeWithWindow", {
                    level: ladderLevel,
                    minutes: ladderWindowMinutes,
                  })
                : tList("ladder.badge", { level: ladderLevel })}
            </Badge>
          )}
          {/* 低速降权：被降了多少 + 是哪个模型把它压下去的。
              多组合时把「还有 N 个」一并说清——单看一个模型名会让人以为只有它在慢。 */}
          {showSlowRateBadge && slowRatePenalty !== null && (
            <Badge
              variant="outline"
              className="flex items-center gap-1 bg-amber-50 text-amber-700 border-amber-300 hover:bg-amber-100 dark:bg-amber-950/40 dark:text-amber-400 dark:border-amber-800"
              title={tList("slowRate.tooltip")}
            >
              <TrendingDown className="h-3 w-3" />
              {slowRateCombinations > 1
                ? tList("slowRate.badgeWithModels", {
                    penalty: slowRatePenalty,
                    model: slowRateModelKey ?? "",
                    count: slowRateCombinations,
                  })
                : tList("slowRate.badge", {
                    penalty: slowRatePenalty,
                    model: slowRateModelKey ?? "",
                  })}
            </Badge>
          )}
          {/* 实时并发数：需在系统设置里开启全局统计（关闭时服务端不报数、本页也不轮询）。
              与并发上限一起报「用了几个 / 上限几个」；未设上限时只报分子。 */}
          {showConcurrencyBadge && concurrencyActive !== null && (
            <Badge
              variant="outline"
              className="flex items-center gap-1 bg-sky-50 text-sky-700 border-sky-300 hover:bg-sky-100 dark:bg-sky-950/40 dark:text-sky-400 dark:border-sky-800"
              title={tList("concurrency.tooltip")}
            >
              <Activity className="h-3 w-3" />
              {concurrencyLimit > 0
                ? tList("concurrency.badgeWithLimit", {
                    count: concurrencyActive,
                    limit: concurrencyLimit,
                  })
                : tList("concurrency.badge", { count: concurrencyActive })}
            </Badge>
          )}
          {/* Endpoint-level circuit badge */}
          {showCircuitBadges && endpointCircuitInfo?.some((ep) => ep.circuitState === "open") && (
            <Badge
              variant="outline"
              className="flex items-center gap-1 bg-orange-100 text-orange-700 border-orange-300 hover:bg-orange-200 dark:bg-orange-900/30 dark:text-orange-400 dark:border-orange-700"
            >
              <AlertTriangle className="h-3 w-3" />
              {tList("endpointCircuitBroken")}
            </Badge>
          )}
          {/* Schedule badge */}
          {provider.activeTimeStart && provider.activeTimeEnd && (
            <Badge variant="outline" className="flex items-center gap-1">
              <Clock className="h-3 w-3" />
              {provider.activeTimeStart}-{provider.activeTimeEnd}
            </Badge>
          )}
          {/* 协议转换标记：仅开启时渲染，未开启不占位 */}
          {provider.protocolConversionEnabled === true && (
            <Badge
              variant="outline"
              className="flex items-center gap-1"
              title={tList("protocolConversion.tooltip")}
            >
              <ArrowRightLeft className="h-3 w-3" />
              {tList("protocolConversion.badge")}
            </Badge>
          )}
        </div>

        {/* Mobile: metrics row */}
        <div className="flex items-center gap-3 text-sm md:hidden">
          <div className="flex items-center gap-1">
            <span className="text-xs text-muted-foreground">{tList("priority")}:</span>
            <span className="font-medium tabular-nums">
              {canEdit ? (
                <PriorityEditPopover
                  globalPriority={provider.priority}
                  groupPriorities={provider.groupPriorities}
                  groups={providerGroups}
                  activeGroupFilter={activeGroupFilter ?? null}
                  validator={validatePriority}
                  onSave={handleSavePriorityWithGroups}
                />
              ) : (
                provider.priority
              )}
            </span>
          </div>
          <div className="flex items-center gap-1">
            <span className="text-xs text-muted-foreground">{tList("weight")}:</span>
            <span className="font-medium tabular-nums">
              {canEdit ? (
                <InlineEditPopover
                  value={provider.weight}
                  label={tInline("weightLabel")}
                  type="integer"
                  validator={validateWeight}
                  onSave={handleSaveWeight}
                />
              ) : (
                provider.weight
              )}
            </span>
          </div>
          <div className="flex items-center gap-1">
            <span className="text-xs text-muted-foreground">{tList("costMultiplier")}:</span>
            <span className="font-medium tabular-nums">
              {canEdit ? (
                <InlineEditPopover
                  value={provider.costMultiplier}
                  label={tInline("costMultiplierLabel")}
                  validator={validateCostMultiplier}
                  onSave={handleSaveCostMultiplier}
                  suffix="x"
                  type="number"
                />
              ) : (
                <>{provider.costMultiplier}x</>
              )}
            </span>
          </div>
        </div>

        {/* Mobile: actions */}
        <div className="flex items-center justify-end gap-2 md:hidden">
          {canEdit && (
            <Button variant="outline" className="min-h-[44px] min-w-[44px]" onClick={handleEdit}>
              <Edit className="h-4 w-4" />
            </Button>
          )}
          {canEdit && (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="outline"
                  className="min-h-[44px] min-w-[44px]"
                  aria-label={tList("actions")}
                >
                  <MoreHorizontal className="h-4 w-4" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={handleClone}>
                  <Copy className="mr-2 h-4 w-4" />
                  {tList("actionClone")}
                </DropdownMenuItem>
                {healthStatus?.circuitState === "open" && (
                  <DropdownMenuItem onClick={handleResetCircuit} disabled={resetPending}>
                    <RotateCcw className="mr-2 h-4 w-4 text-orange-600" />
                    {tList("actionResetCircuit")}
                  </DropdownMenuItem>
                )}
                {provider.limitTotalUsd !== null && provider.limitTotalUsd > 0 && (
                  <DropdownMenuItem onClick={handleResetTotalUsage} disabled={resetUsagePending}>
                    <RotateCcw className="mr-2 h-4 w-4 text-blue-600" />
                    {tList("actionResetUsage")}
                  </DropdownMenuItem>
                )}
                {/* 熔断日志：**任何供应商都可查看**（未熔断的排障同样需要）。
                    用 onSelect 而不是 onClick：Radix 的菜单项在选中后立即关闭菜单，
                    菜单卸载会连带卸载触发它的 Dialog——故此处只置状态，
                    Dialog 本体挂在菜单之外（见文件末尾的受控挂载）。 */}
                <DropdownMenuItem onSelect={() => setCircuitLogsOpen(true)}>
                  <ScrollText className="mr-2 h-4 w-4" />
                  {tList("actionCircuitLogs")}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  className="text-destructive"
                  onSelect={() => setMobileDeleteDialogOpen(true)}
                >
                  <Trash className="mr-2 h-4 w-4" />
                  {tList("actionDelete")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </div>

        {/* 移动端的熔断日志：受控挂在菜单**之外**。
            若把 Dialog 放在 DropdownMenuItem 里，Radix 选中后卸载菜单会连带卸载该 Dialog，
            弹窗一闪即消（这是本组件里已有的教训形状：删除确认也是同一处理——
            见上面的 mobileDeleteDialogOpen）。 */}
        {canEdit && (
          <ProviderCircuitLogsDialog
            providerId={provider.id}
            providerName={provider.name}
            open={circuitLogsOpen}
            onOpenChange={setCircuitLogsOpen}
          />
        )}

        {canEdit && (
          <AlertDialog open={mobileDeleteDialogOpen} onOpenChange={setMobileDeleteDialogOpen}>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>{tList("confirmDeleteTitle")}</AlertDialogTitle>
                <AlertDialogDescription>
                  {tList("confirmDeleteMessage", { name: provider.name })}
                </AlertDialogDescription>
              </AlertDialogHeader>
              <div className="flex justify-end gap-2">
                <AlertDialogCancel>{tList("cancelButton")}</AlertDialogCancel>
                <AlertDialogAction
                  onClick={handleDelete}
                  className="bg-red-600 hover:bg-red-700"
                  disabled={deletePending}
                >
                  {tList("deleteButton")}
                </AlertDialogAction>
              </div>
            </AlertDialogContent>
          </AlertDialog>
        )}

        {/* Desktop: original info section (hidden on mobile) */}
        <div className="hidden md:flex items-center gap-2 flex-shrink-0">
          {provider.isEnabled ? (
            <CheckCircle className="h-4 w-4 text-green-500 flex-shrink-0" />
          ) : (
            <XCircle className="h-4 w-4 text-gray-400 flex-shrink-0" />
          )}
          <div
            className={`flex items-center justify-center w-6 h-6 rounded ${typeConfig.bgColor} flex-shrink-0`}
            title={`${typeLabel} - ${typeDescription}`}
            aria-label={typeLabel}
          >
            <TypeIcon className="h-3.5 w-3.5" aria-hidden />
          </div>
        </div>

        <div className="hidden md:block flex-1 min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            {provider.faviconUrl && (
              <img
                src={provider.faviconUrl}
                alt=""
                className="h-4 w-4 flex-shrink-0"
                onError={(e) => {
                  (e.target as HTMLImageElement).style.display = "none";
                }}
              />
            )}
            <span className="font-semibold truncate">{provider.name}</span>
            {canEdit ? (
              <GroupEditCombobox
                currentGroups={providerGroups}
                allGroups={allGroups}
                userGroups={userGroups}
                isAdmin={isAdmin}
                onSave={handleSaveGroups}
              />
            ) : providerGroups.length > 0 ? (
              providerGroups.map((tag, index) => {
                const bgColor = getGroupColor(tag);
                return (
                  <Badge
                    key={`${tag}-${index}`}
                    className="flex-shrink-0 text-xs"
                    style={{ backgroundColor: bgColor, color: getContrastTextColor(bgColor) }}
                  >
                    {tag}
                  </Badge>
                );
              })
            ) : (
              <Badge variant="outline" className="flex-shrink-0">
                {PROVIDER_GROUP.DEFAULT}
              </Badge>
            )}
            {/* Key-level circuit badge */}
            {showCircuitBadges && healthStatus?.circuitState === "open" && (
              <Badge variant="destructive" className="flex items-center gap-1 flex-shrink-0">
                <AlertTriangle className="h-3 w-3" />
                {tList("keyCircuitBroken")}
              </Badge>
            )}
            {/* 半开：见桌面端的同款说明（窗口过期、已放行试探，不得报成「熔断」）。 */}
            {showCircuitBadges && healthStatus?.circuitState === "half-open" && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0 bg-amber-100 text-amber-700 border-amber-300 hover:bg-amber-200 dark:bg-amber-900/30 dark:text-amber-400 dark:border-amber-700"
              >
                <AlertTriangle className="h-3 w-3" />
                {tList("keyCircuitHalfOpen")}
              </Badge>
            )}
            {/* 等待阶梯（桌面端同款，见那里的说明）。 */}
            {showLadderBadge && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0 bg-sky-100 text-sky-700 border-sky-300 hover:bg-sky-200 dark:bg-sky-900/30 dark:text-sky-400 dark:border-sky-700"
                title={tList("ladder.tooltip")}
              >
                <TrendingUp className="h-3 w-3" />
                {ladderWindowMinutes !== null
                  ? tList("ladder.badgeWithWindow", {
                      level: ladderLevel,
                      minutes: ladderWindowMinutes,
                    })
                  : tList("ladder.badge", { level: ladderLevel })}
              </Badge>
            )}
            {/* 低速降权（桌面端同款，见那里的说明）。 */}
            {showSlowRateBadge && slowRatePenalty !== null && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0 bg-amber-50 text-amber-700 border-amber-300 hover:bg-amber-100 dark:bg-amber-950/40 dark:text-amber-400 dark:border-amber-800"
                title={tList("slowRate.tooltip")}
              >
                <TrendingDown className="h-3 w-3" />
                {slowRateCombinations > 1
                  ? tList("slowRate.badgeWithModels", {
                      penalty: slowRatePenalty,
                      model: slowRateModelKey ?? "",
                      count: slowRateCombinations,
                    })
                  : tList("slowRate.badge", {
                      penalty: slowRatePenalty,
                      model: slowRateModelKey ?? "",
                    })}
              </Badge>
            )}
            {/* 实时并发数（桌面端同款，见那里的说明）。 */}
            {showConcurrencyBadge && concurrencyActive !== null && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0 bg-sky-50 text-sky-700 border-sky-300 hover:bg-sky-100 dark:bg-sky-950/40 dark:text-sky-400 dark:border-sky-800"
                title={tList("concurrency.tooltip")}
              >
                <Activity className="h-3 w-3" />
                {concurrencyLimit > 0
                  ? tList("concurrency.badgeWithLimit", {
                      count: concurrencyActive,
                      limit: concurrencyLimit,
                    })
                  : tList("concurrency.badge", { count: concurrencyActive })}
              </Badge>
            )}
            {/* Endpoint-level circuit badge */}
            {showCircuitBadges && endpointCircuitInfo?.some((ep) => ep.circuitState === "open") && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0 bg-orange-100 text-orange-700 border-orange-300 hover:bg-orange-200 dark:bg-orange-900/30 dark:text-orange-400 dark:border-orange-700"
              >
                <AlertTriangle className="h-3 w-3" />
                {tList("endpointCircuitBroken")}
              </Badge>
            )}
            {/* Schedule badge */}
            {provider.activeTimeStart && provider.activeTimeEnd && (
              <Badge variant="outline" className="flex items-center gap-1 flex-shrink-0">
                <Clock className="h-3 w-3" />
                {provider.activeTimeStart}-{provider.activeTimeEnd}
              </Badge>
            )}
            {/* 协议转换标记：仅开启时渲染，未开启不占位 */}
            {provider.protocolConversionEnabled === true && (
              <Badge
                variant="outline"
                className="flex items-center gap-1 flex-shrink-0"
                title={tList("protocolConversion.tooltip")}
              >
                <ArrowRightLeft className="h-3 w-3" />
                {tList("protocolConversion.badge")}
              </Badge>
            )}
          </div>
          <div className="flex items-center gap-3 mt-1 text-sm text-muted-foreground flex-wrap">
            {/* Vendor & Endpoints OR Legacy URL */}
            {vendor ? (
              <div className="flex items-center gap-2">
                <span className="truncate max-w-[300px] font-medium text-foreground/80">
                  {vendor.displayName || vendor.websiteDomain}
                </span>
                <ProviderEndpointHover vendorId={vendor.id} providerType={provider.providerType} />
              </div>
            ) : (
              <span className="truncate max-w-[300px]">{provider.url}</span>
            )}
            {provider.proxyUrl && (
              <Tooltip delayDuration={200}>
                <TooltipTrigger asChild>
                  <span className="inline-flex cursor-help">
                    <ShieldCheck className="h-3.5 w-3.5 text-emerald-500" />
                  </span>
                </TooltipTrigger>
                <TooltipContent side="top" className="text-xs">
                  {tList("proxyEnabled")}
                </TooltipContent>
              </Tooltip>
            )}

            {/* 官网链接 */}
            {provider.websiteUrl && (
              <a
                href={provider.websiteUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 hover:underline text-blue-600 hover:text-blue-700 flex-shrink-0"
                onClick={(e) => e.stopPropagation()}
              >
                <Globe className="h-3 w-3" />
                {tList("officialWebsite")}
              </a>
            )}
            {canEdit && (
              <button
                onClick={(e) => {
                  e.stopPropagation();
                  handleShowKey();
                }}
                className="inline-flex items-center gap-1 text-xs font-mono hover:underline flex-shrink-0"
              >
                <Key className="h-3 w-3" />
                {provider.maskedKey}
              </button>
            )}
            <span className="text-xs text-muted-foreground flex-shrink-0">
              {tTimeout("summary", {
                streaming: (
                  (provider.firstByteTimeoutStreamingMs ??
                    PROVIDER_TIMEOUT_DEFAULTS.FIRST_BYTE_TIMEOUT_STREAMING_MS) / 1000
                ).toString(),
                idle: (
                  (provider.streamingIdleTimeoutMs ??
                    PROVIDER_TIMEOUT_DEFAULTS.STREAMING_IDLE_TIMEOUT_MS) / 1000
                ).toString(),
                nonStreaming: (
                  (provider.requestTimeoutNonStreamingMs ??
                    PROVIDER_TIMEOUT_DEFAULTS.REQUEST_TIMEOUT_NON_STREAMING_MS) / 1000
                ).toString(),
              })}
            </span>
          </div>
        </div>

        {/* Desktop: metrics */}
        <div className="hidden md:grid grid-cols-3 gap-2 text-center flex-shrink-0">
          <div className="rounded-md bg-muted/30 px-2.5 py-1.5">
            <div className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
              {tList("priority")}
            </div>
            <div className="font-semibold text-sm">
              {canEdit ? (
                <PriorityEditPopover
                  globalPriority={provider.priority}
                  groupPriorities={provider.groupPriorities}
                  groups={providerGroups}
                  activeGroupFilter={activeGroupFilter ?? null}
                  validator={validatePriority}
                  onSave={handleSavePriorityWithGroups}
                />
              ) : (
                <span>{provider.priority}</span>
              )}
            </div>
          </div>
          <div className="rounded-md bg-muted/30 px-2.5 py-1.5">
            <div className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
              {tList("weight")}
            </div>
            <div className="font-semibold text-sm">
              {canEdit ? (
                <InlineEditPopover
                  value={provider.weight}
                  label={tInline("weightLabel")}
                  type="integer"
                  validator={validateWeight}
                  onSave={handleSaveWeight}
                />
              ) : (
                <span>{provider.weight}</span>
              )}
            </div>
          </div>
          <div className="rounded-md bg-muted/30 px-2.5 py-1.5">
            <div className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
              {tList("costMultiplier")}
            </div>
            <div className="font-semibold text-sm">
              {canEdit ? (
                <InlineEditPopover
                  value={provider.costMultiplier}
                  label={tInline("costMultiplierLabel")}
                  validator={validateCostMultiplier}
                  onSave={handleSaveCostMultiplier}
                  suffix="x"
                  type="number"
                />
              ) : (
                <span>{provider.costMultiplier}x</span>
              )}
            </div>
          </div>
        </div>

        {/* Desktop: today usage */}
        <div className="hidden lg:block text-center flex-shrink-0 min-w-[100px] rounded-md bg-muted/30 px-2.5 py-1.5">
          <div className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
            {tList("todayUsageLabel")}
          </div>
          {statisticsLoading ? (
            <>
              <Skeleton className="h-5 w-16 mx-auto my-0.5" />
              <Skeleton className="h-4 w-12 mx-auto mt-0.5" />
            </>
          ) : (
            <>
              <div className="font-semibold text-sm">
                {tList("todayUsageCount", {
                  count: statistics?.todayCalls ?? provider.todayCallCount ?? 0,
                })}
              </div>
              <div className="text-xs font-mono text-muted-foreground mt-0.5">
                {formatCurrency(
                  parseFloat(statistics?.todayCost ?? provider.todayTotalCostUsd ?? "0"),
                  currencyCode
                )}
              </div>
            </>
          )}
        </div>

        {/* Desktop: action buttons */}
        <div className="hidden md:flex items-center gap-1 flex-shrink-0">
          {canEdit && (
            <Switch
              aria-label={provider.name}
              checked={provider.isEnabled}
              onCheckedChange={handleToggle}
              disabled={togglePending}
              className="data-[state=checked]:bg-green-500"
            />
          )}
          {canEdit && (
            <Button
              size="icon"
              variant="ghost"
              onClick={(e) => {
                e.stopPropagation();
                handleEdit();
              }}
              disabled={!canEdit}
            >
              <Edit className="h-4 w-4" />
            </Button>
          )}
          {canEdit && (
            <Button
              size="icon"
              variant="ghost"
              onClick={(e) => {
                e.stopPropagation();
                handleClone();
              }}
              disabled={!canEdit}
            >
              <Copy className="h-4 w-4" />
            </Button>
          )}
          {canEdit && healthStatus?.circuitState === "open" && (
            <Button
              size="icon"
              variant="ghost"
              onClick={(e) => {
                e.stopPropagation();
                handleResetCircuit();
              }}
              disabled={resetPending}
            >
              <RotateCcw className="h-4 w-4 text-orange-600" />
            </Button>
          )}
          {canEdit && provider.limitTotalUsd !== null && provider.limitTotalUsd > 0 && (
            <Button
              size="icon"
              variant="ghost"
              title={tList("resetUsageTitle")}
              onClick={(e) => {
                e.stopPropagation();
                handleResetTotalUsage();
              }}
              disabled={resetUsagePending}
            >
              <RotateCcw className="h-4 w-4 text-blue-600" />
            </Button>
          )}
          {/* 熔断日志：与「重置熔断」相邻，因为它们是同一场景的两半——
              先看日志弄清楚为什么断，再决定要不要重置。 */}
          {canEdit && (
            <ProviderCircuitLogsDialog
              providerId={provider.id}
              providerName={provider.name}
              trigger={
                <Button
                  size="icon"
                  variant="ghost"
                  title={tList("actionCircuitLogsTitle")}
                  onClick={(e) => e.stopPropagation()}
                >
                  <ScrollText className="h-4 w-4" />
                </Button>
              }
            />
          )}
          {canEdit && (
            <AlertDialog>
              <AlertDialogTrigger asChild>
                <Button
                  size="icon"
                  variant="ghost"
                  onClick={(e) => e.stopPropagation()}
                  disabled={!canEdit}
                >
                  <Trash className="h-4 w-4 text-red-600" />
                </Button>
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{tList("confirmDeleteTitle")}</AlertDialogTitle>
                  <AlertDialogDescription>
                    {tList("confirmDeleteMessage", { name: provider.name })}
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <div className="flex justify-end gap-2">
                  <AlertDialogCancel>{tList("cancelButton")}</AlertDialogCancel>
                  <AlertDialogAction
                    onClick={(e) => {
                      e.stopPropagation();
                      handleDelete();
                    }}
                    className="bg-red-600 hover:bg-red-700"
                    disabled={deletePending}
                  >
                    {tList("deleteButton")}
                  </AlertDialogAction>
                </div>
              </AlertDialogContent>
            </AlertDialog>
          )}
        </div>
      </div>

      {/* Edit Dialog */}
      <Dialog open={openEdit} onOpenChange={setOpenEdit}>
        <ProviderFormDialogContent className="max-w-6xl">
          <VisuallyHidden>
            <DialogTitle>{t("editProvider")}</DialogTitle>
          </VisuallyHidden>
          {editFormReady ? (
            <FormErrorBoundary>
              <ProviderForm
                mode="edit"
                provider={provider}
                onSuccess={() => {
                  setOpenEdit(false);
                }}
                enableMultiProviderTypes={enableMultiProviderTypes}
              />
            </FormErrorBoundary>
          ) : (
            <DialogFormSkeleton />
          )}
        </ProviderFormDialogContent>
      </Dialog>

      {/* Clone Dialog */}
      <Dialog open={openClone} onOpenChange={setOpenClone}>
        <ProviderFormDialogContent className="max-w-6xl">
          <VisuallyHidden>
            <DialogTitle>{t("clone")}</DialogTitle>
          </VisuallyHidden>
          {cloneFormReady ? (
            <FormErrorBoundary>
              <ProviderForm
                mode="create"
                cloneProvider={provider}
                onSuccess={() => {
                  setOpenClone(false);
                }}
                enableMultiProviderTypes={enableMultiProviderTypes}
              />
            </FormErrorBoundary>
          ) : (
            <DialogFormSkeleton />
          )}
        </ProviderFormDialogContent>
      </Dialog>

      {/* API Key 展示 Dialog */}
      <Dialog open={showKeyDialog} onOpenChange={handleCloseDialog}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>{tList("viewFullKey")}</DialogTitle>
            <DialogDescription>{tList("viewFullKeyDesc")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="flex items-center gap-2">
              <code className="flex-1 font-mono bg-muted px-3 py-2 rounded text-sm break-all">
                {unmaskedKey || tList("keyLoading")}
              </code>
              {clipboardAvailable && (
                <Button onClick={handleCopy} disabled={!unmaskedKey} size="icon" variant="outline">
                  {copied ? (
                    <CheckCircle className="h-4 w-4 text-green-600" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              )}
            </div>
            {!clipboardAvailable && (
              <p className="text-xs text-muted-foreground">{tList("clipboardUnavailable")}</p>
            )}
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}

export const ProviderRichListItem = memo(ProviderRichListItemInner, (prev, next) => {
  // Skip function props — they are safe to ignore because:
  // - onSelectProvider (parent) uses useCallback + functional setState (prev => ...)
  // - onSelectChange (ProviderList) binds stable provider.id to the above
  // - onEdit/onClone/onDelete are undefined in ProviderList (use internal handlers)
  // If any of these assumptions change, add the relevant prop to this comparison.
  return (
    prev.provider === next.provider &&
    prev.vendor === next.vendor &&
    prev.currentUser === next.currentUser &&
    prev.healthStatus === next.healthStatus &&
    prev.endpointCircuitInfo === next.endpointCircuitInfo &&
    prev.statistics === next.statistics &&
    prev.statisticsLoading === next.statisticsLoading &&
    prev.currencyCode === next.currencyCode &&
    prev.enableMultiProviderTypes === next.enableMultiProviderTypes &&
    prev.isMultiSelectMode === next.isMultiSelectMode &&
    prev.isSelected === next.isSelected &&
    prev.activeGroupFilter === next.activeGroupFilter &&
    prev.allGroups === next.allGroups &&
    prev.userGroups === next.userGroups &&
    prev.isAdmin === next.isAdmin
  );
});

/** Lightweight placeholder shown while ProviderForm mounts (keeps dialog animation smooth) */
function DialogFormSkeleton() {
  return (
    <div className="flex flex-col h-[60vh] animate-pulse">
      <div className="flex flex-1 min-h-0">
        <div className="hidden lg:block w-48 shrink-0 border-r p-4 space-y-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-8 w-full" />
          ))}
        </div>
        <div className="flex-1 p-6 space-y-6">
          <Skeleton className="h-6 w-48" />
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-6 w-36 mt-4" />
          <Skeleton className="h-10 w-full" />
        </div>
      </div>
      <div className="shrink-0 px-6 py-4 border-t">
        <Skeleton className="h-10 w-24 ml-auto" />
      </div>
    </div>
  );
}
