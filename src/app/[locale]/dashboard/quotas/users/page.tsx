"use client";

import { useQuery } from "@tanstack/react-query";
import { Info } from "lucide-react";
import { useTranslations } from "next-intl";
import { QuotaToolbar } from "@/components/quota/quota-toolbar";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { UiSessionGate } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { getKeyCostBatch, getUserCostBatch } from "@/lib/api-client/v1/actions/cost-batch";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { getUserLimitUsage, getUsersBatchCore } from "@/lib/api-client/v1/actions/users";
import { UsersQuotaSkeleton } from "../_components/users-quota-skeleton";
import type { UserKeyWithUsage, UserQuotaWithUsage } from "./_components/types";
import { UsersQuotaClient } from "./_components/users-quota-client";

/** 与改造前的 SSR 版同口径：上限 2000 个用户。
 *
 * 每页 **100**（不是 SSR 时代的 200）：REST 端点 `GET /users` 的 `limit` 上限就是 100
 * （Node `UserListQuerySchema` 与 Go `parseUsersListQuery` 两侧一致，均为 min 1 / max 100 / default 50）。
 * 改造前的 SSR 版直调内部 action、不经 HTTP 校验，故 200 能用；改走 REST 后 200 会被两后端
 * 同样拒为 `too_big`，整页取不到用户。总量上限仍由下面的游标循环封在 2000。 */
const MAX_USERS_FOR_QUOTAS = 2000;
const USERS_PAGE_SIZE = 100;
const MAX_ITERATIONS = Math.ceil(MAX_USERS_FOR_QUOTAS / USERS_PAGE_SIZE) + 1;
/**
 * 限额读数的并发上限。改造前是服务端 `Promise.all(users.map(getUserLimitUsage))`（进程内调用），
 * 搬到浏览器后同样的写法会一次性打 2000 个 HTTP 请求，故改为分批。
 */
const QUOTA_FETCH_CONCURRENCY = 20;

/**
 * 用户配额页。改造前是 SSR：`getSession()` 判 admin + 分页拉用户 + 逐用户限额读数
 * + 服务端聚合累计成本。静态导出后无服务端，故数据全部走 REST。
 *
 * 权限口径不变：仅 admin；非 admin 已登录 → /dashboard/my-quota，未登录 → /login。
 *
 * **累计成本来自 Go 侧的批量端点**：`POST /api/v1/users:costBatch` 与 `POST /api/v1/keys:costBatch`
 * （本页改造时为缺失能力，靠 SSR 直接查库绕过；现在有了 REST 面）。
 * 取数口径（表 `usage_ledger`、计费条件、**全时段无时间上限**、按 `cost_reset_at` 起算）
 * 与 Node 的 `sumUserTotalCostBatch` / `sumKeyTotalCostBatchByIds` 一致，逐条对照见
 * `go/internal/store/admin_cost_batch.go` 的文件注释与 ``。
 * **取数失败时这两个字段为 `null`（读数不可用）而不是 0**：0 会被读成「确实没花钱」，
 * 连带额度进度条与按成本排序一起失真。页面必须显式说出「读数不可用」。
 * 其余列（限额、当日用量、密钥、分组、有效期）均为真实读数。
 */
export default function UsersQuotaPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard/my-quota">
      <UsersQuotaContent />
    </UiSessionGate>
  );
}

async function fetchUsersWithQuotas(): Promise<UserQuotaWithUsage[]> {
  const collected: UserQuotaWithUsage[] = [];
  const users: {
    id: number;
    name: string;
    note?: string;
    role: "admin" | "user";
    isEnabled: boolean;
    expiresAt: Date | null;
    providerGroup?: string | null;
    tags?: string[];
    limit5hUsd?: number | null;
    limitWeeklyUsd?: number | null;
    limitMonthlyUsd?: number | null;
    limitTotalUsd?: number | null;
    limitConcurrentSessions?: number | null;
    keys: UserKeyWithUsage[];
  }[] = [];
  let cursor: string | undefined;
  let iterations = 0;

  while (users.length < MAX_USERS_FOR_QUOTAS && iterations < MAX_ITERATIONS) {
    iterations += 1;
    const page = await getUsersBatchCore({ cursor, limit: USERS_PAGE_SIZE });
    if (!page.ok) throw new Error(page.error ?? "failed to load users");

    for (const user of page.data.users) {
      users.push({
        id: user.id,
        name: user.name,
        note: user.note,
        role: user.role,
        isEnabled: user.isEnabled,
        expiresAt: user.expiresAt ? new Date(user.expiresAt) : null,
        providerGroup: user.providerGroup ?? null,
        tags: user.tags,
        limit5hUsd: user.limit5hUsd ?? null,
        limitWeeklyUsd: user.limitWeeklyUsd ?? null,
        limitMonthlyUsd: user.limitMonthlyUsd ?? null,
        limitTotalUsd: user.limitTotalUsd ?? null,
        limitConcurrentSessions: user.limitConcurrentSessions ?? null,
        keys: user.keys.map((key) => ({
          id: key.id,
          name: key.name,
          status: key.status,
          todayUsage: key.todayUsage,
          // 先置 null（读数不可用）；下文的批量成本回填真实值。
          totalUsage: null,
          limit5hUsd: key.limit5hUsd ?? null,
          limitDailyUsd: key.limitDailyUsd ?? null,
          limitWeeklyUsd: key.limitWeeklyUsd ?? null,
          limitMonthlyUsd: key.limitMonthlyUsd ?? null,
          limitTotalUsd: key.limitTotalUsd ?? null,
          limitConcurrentSessions: key.limitConcurrentSessions ?? 0,
          dailyResetMode: key.dailyResetMode,
          dailyResetTime: key.dailyResetTime,
        })),
      });
    }

    if (!page.data.hasMore || !page.data.nextCursor) break;
    cursor = page.data.nextCursor;
  }

  // 批量累计成本（两条端点各一次或数次，见 cost-batch.ts 的分片）。
  //
  // 失败时**不降级成 0**：0 会被读成「确实没花钱」，连带额度进度条与按成本排序一起失真；
  // 故整列置 null（读数不可用），由界面显式说明。这与逐用户限额读数（失败即 null）同一口径。
  const userIds = users.map((user) => user.id);
  const keyIds = users.flatMap((user) => user.keys.map((key) => key.id));
  const [userCosts, keyCosts] = await Promise.all([
    getUserCostBatch(userIds).catch(() => null),
    getKeyCostBatch(keyIds).catch(() => null),
  ]);

  for (let start = 0; start < users.length; start += QUOTA_FETCH_CONCURRENCY) {
    const chunk = users.slice(start, start + QUOTA_FETCH_CONCURRENCY);
    const snapshots = await Promise.all(
      chunk.map((user) => getUserLimitUsage(user.id).catch(() => null))
    );
    chunk.forEach((user, index) => {
      const snapshot = snapshots[index];
      collected.push({
        id: user.id,
        name: user.name,
        note: user.note,
        role: user.role,
        isEnabled: user.isEnabled,
        expiresAt: user.expiresAt,
        providerGroup: user.providerGroup,
        tags: user.tags,
        quota: snapshot?.ok ? snapshot.data : null,
        limit5hUsd: user.limit5hUsd ?? null,
        limitWeeklyUsd: user.limitWeeklyUsd ?? null,
        limitMonthlyUsd: user.limitMonthlyUsd ?? null,
        limitTotalUsd: user.limitTotalUsd ?? null,
        limitConcurrentSessions: user.limitConcurrentSessions ?? null,
        totalUsage: userCosts?.get(user.id) ?? null,
        keys: user.keys.map((key) => ({
          ...key,
          totalUsage: keyCosts?.get(key.id) ?? null,
        })),
      });
    });
  }

  return collected;
}

function UsersQuotaContent() {
  const t = useTranslations("quota.users");

  const users = useQuery({
    queryKey: ["v1", "quotas", "users"],
    queryFn: fetchUsersWithQuotas,
  });
  const settings = useQuery({
    queryKey: ["v1", "system", "settings"],
    queryFn: () => getSystemSettings(),
  });

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-lg font-medium">{t("title")}</h3>
        </div>
      </div>

      <Alert>
        <Info className="h-4 w-4" />
        <AlertDescription>
          {t("manageNotice")}{" "}
          <Link href="/dashboard/users" className="font-medium underline underline-offset-4">
            {t("manageLink")}
          </Link>
        </AlertDescription>
      </Alert>

      <QuotaToolbar
        sortOptions={[
          { value: "name", label: t("sort.name") },
          { value: "usage", label: t("sort.usage") },
        ]}
        filterOptions={[
          { value: "all", label: t("filter.all") },
          { value: "warning", label: t("filter.warning") },
          { value: "exceeded", label: t("filter.exceeded") },
        ]}
      />

      {users.isPending || settings.isPending ? (
        <UsersQuotaSkeleton />
      ) : users.isError ? (
        <p className="text-sm text-destructive">
          {users.error instanceof Error ? users.error.message : t("title")}
        </p>
      ) : (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">
            {t("totalCount", { count: users.data.length })}
          </p>
          <UsersQuotaClient users={users.data} currencyCode={settings.data?.currencyDisplay} />
        </div>
      )}
    </div>
  );
}
