/**
 * Timezone Utilities
 *
 * Provides timezone validation and resolution functions.
 * Uses IANA timezone database identifiers (e.g., "Asia/Shanghai", "America/New_York").
 *
 * resolveSystemTimezone() implements the fallback chain:
 *   DB timezone -> env TZ -> UTC
 */

import { getEnvConfig } from "@/lib/config/env.schema";
import { logger } from "@/lib/logger";
import { isValidIANATimezone } from "@/lib/utils/timezone-shared";

export {
  COMMON_TIMEZONES,
  type CommonTimezone,
  getTimezoneLabel,
  getTimezoneOffsetMinutes,
  isValidIANATimezone,
} from "./timezone-shared";

/**
 * Resolves the system timezone using the fallback chain:
 *   1. env TZ variable
 *   2. "UTC" as final fallback
 *
 * Node 退役后不再读 DB：时区的权威值由 Go 侧承担（`internal/config` 的 TZ 项，默认 Asia/Shanghai），
 * 本函数只保留「构建期/静态导出可用」的来源。生产容器设了 TZ，故行为与退役前一致。
 *
 * Each candidate is validated via isValidIANATimezone before being accepted.
 *
 * @returns Resolved IANA timezone identifier (always valid)
 */
export async function resolveSystemTimezone(): Promise<string> {
  // Step 1: Fallback to env TZ
  try {
    const { TZ } = getEnvConfig();
    if (TZ && isValidIANATimezone(TZ)) {
      return TZ;
    }
  } catch (error) {
    logger.warn("[TimezoneResolver] Failed to read env TZ", { error });
  }

  // Step 2: Ultimate fallback
  return "UTC";
}
