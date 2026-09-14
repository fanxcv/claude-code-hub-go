import { logger } from "@/lib/logger";
import { DEFAULT_SITE_TITLE as FALLBACK_SITE_TITLE } from "@/lib/site-title";

export async function resolveDefaultSiteMetadataSource(): Promise<{
  siteTitle: string;
  siteDescription: string;
}> {
  // Node 退役后不再读 DB 系统设置：站点标题的权威值在 Go 侧（system_settings），
  // 而静态导出在构建期就把 metadata 固化了，运行期能覆盖它的只有 Go 的壳注入。
  // 这里返回构建期兜底，与退役前 catch 分支同义（详见报告的行为变化登记）。
  return {
    siteTitle: FALLBACK_SITE_TITLE,
    siteDescription: FALLBACK_SITE_TITLE,
  };
}

export async function resolveDefaultLayoutTimeZone(): Promise<string> {
  try {
    const { resolveSystemTimezone } = await import("@/lib/utils/timezone");
    return await resolveSystemTimezone();
  } catch (error) {
    logger.warn("resolveDefaultLayoutTimeZone failed", {
      error: error instanceof Error ? error.message : String(error),
    });
    return "UTC";
  }
}
