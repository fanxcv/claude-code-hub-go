/**
 * public-status 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/actions/public-status.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { PublicStatusModelConfig } from "@/lib/public-status/config";
export interface SavePublicStatusSettingsInput {
  publicStatusWindowHours: number;
  publicStatusAggregationIntervalMinutes: number;
  groups: Array<{
    groupName: string;
    displayName?: string;
    publicGroupSlug?: string;
    explanatoryCopy?: string | null;
    sortOrder?: number;
    publicModels: PublicStatusModelConfig[];
  }>;
}
