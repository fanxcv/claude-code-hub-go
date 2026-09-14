/**
 * request-filters 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/request-filters.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { FilterOperation } from "@/lib/request-filter-types";
export interface RequestFilter {
  id: number;
  name: string;
  description: string | null;
  scope: RequestFilterScope;
  action: RequestFilterAction;
  matchType: RequestFilterMatchType;
  target: string;
  replacement: unknown;
  priority: number;
  isEnabled: boolean;
  bindingType: RequestFilterBindingType;
  providerIds: number[] | null;
  groupTags: string[] | null;
  ruleMode: RequestFilterRuleMode;
  executionPhase: RequestFilterExecutionPhase;
  operations: FilterOperation[] | null;
  createdAt: Date;
  updatedAt: Date;
}

export type RequestFilterBindingType = "global" | "providers" | "groups";

export type RequestFilterExecutionPhase = "guard" | "final";

export type RequestFilterMatchType = "regex" | "contains" | "exact" | null;

export type RequestFilterRuleMode = "simple" | "advanced";

export type RequestFilterScope = "header" | "body";

export type RequestFilterAction = "remove" | "set" | "json_path" | "text_replace";
