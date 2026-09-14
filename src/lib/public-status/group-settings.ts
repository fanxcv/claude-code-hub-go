import type { PublicStatusModelConfig } from "@/lib/public-status/config";
import {
  createUniquePublicGroupSlug,
  normalizePublicGroupSlug,
  parsePublicStatusDescription,
} from "@/lib/public-status/config";

/** 公开状态页设置表单需要的分组输入（与 `public-status-settings-form` 的 props 同名同形）。 */
export interface StatusPageGroupInput {
  groupName: string;
  enabled: boolean;
  displayName: string;
  publicGroupSlug: string;
  explanatoryCopy: string;
  sortOrder: number;
  publicModels: PublicStatusModelConfig[];
}

/**
 * 由「分组行」推导公开状态页设置表单的初始分组列表。
 *
 * 抽成纯函数是为了让两处调用方共用一份口径：
 * - 服务端 loader（`settings/status-page/loader.ts`，Node 仍在跑时的 SSR 路径）；
 * - 静态化后的客户端页（`settings/status-page/page.tsx`）。
 * 两边各写一遍 slug 去重与默认值推导，迟早会漂移出「同一分组两个 slug」这类静默错数。
 */
export function deriveStatusPageGroupInputs(
  groups: Array<{ name: string; description: string | null }>
): StatusPageGroupInput[] {
  const parsedGroups = groups.map((group) => ({
    group,
    parsed: parsePublicStatusDescription(group.description),
  }));

  const usedDefaultSlugs = new Set<string>();
  for (const { group, parsed } of parsedGroups) {
    if (parsed.publicStatus?.publicGroupSlug) {
      usedDefaultSlugs.add(
        normalizePublicGroupSlug(group.name, parsed.publicStatus.publicGroupSlug)
      );
    }
  }

  return parsedGroups.map(({ group, parsed }) => ({
    groupName: group.name,
    enabled: (parsed.publicStatus?.publicModels.length ?? 0) > 0,
    displayName: parsed.publicStatus?.displayName ?? "",
    publicGroupSlug:
      parsed.publicStatus?.publicGroupSlug ??
      createUniquePublicGroupSlug(group.name, usedDefaultSlugs),
    explanatoryCopy: parsed.publicStatus?.explanatoryCopy ?? "",
    sortOrder: parsed.publicStatus?.sortOrder ?? 0,
    publicModels: parsed.publicStatus?.publicModels ?? [],
  }));
}
