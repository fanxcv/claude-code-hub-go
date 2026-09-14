/**
 * Provider Availability Module
 *
 * Read path aggregates pre-projected 1-minute buckets (avail_bucket_1m / avail_current).
 * Write path finalization still relies on message_request.statusCode: a DB trigger enqueues
 * outbox events, and the in-process projection worker increments the buckets.
 * In-flight / intermediate records never enter the projection.
 *
 * 1. HTTP Status Check: 2xx/3xx = success (green), other finalized HTTP status codes = failure (red)
 *
 * Availability scoring:
 * - GREEN (1.0): Successful requests (any HTTP 2xx/3xx)
 * - RED (0.0): Failed finalized requests (non-2xx/3xx HTTP status codes)
 * - UNKNOWN: No data available
 */

// Node 退役后，本模块只剩**展示契约**：原先的值导出（queryProviderAvailability /
// calculateAvailabilityScore / classifyRequestStatus / getCurrentProviderStatus /
// AvailabilityQueryValidationError 与各 MAX_/MIN_ 常量）都来自 availability-service，
// 而它依赖已删的 @/drizzle/db 与 @/drizzle/schema（读 avail_bucket_1m 等投影表）。
// 可用性数据现由 Go 提供（REST 面），UI 只消费其类型。
export * from "./types";
