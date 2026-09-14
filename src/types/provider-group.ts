/**
 * Provider group entity.
 * Maps to the provider_groups table.
 */
export interface ProviderGroup {
  id: number;
  name: string;
  costMultiplier: number;
  description: string | null;
  createdAt: Date;
  updatedAt: Date;
}

/**
 * Input for creating a new provider group.
 */
export interface CreateProviderGroupInput {
  name: string;
  costMultiplier?: number;
  description?: string | null;
}

/**
 * Input for updating a provider group.
 */
export interface UpdateProviderGroupInput {
  costMultiplier?: number;
  description?: string | null;
}

// ---- 2026-09 node 退役迁移（原 actions/provider-groups） ----
// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type ProviderGroupWithCount = ProviderGroup & {
  providerCount: number;
};
