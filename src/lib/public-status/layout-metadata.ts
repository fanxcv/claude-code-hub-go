import {
  resolveDefaultLayoutTimeZone,
  resolveDefaultSiteMetadataSource,
} from "@/lib/layout-site-metadata";

/**
 * 公开状态请求（`x-cch-public-status: 1`）专用的站点元数据来源解析。
 *
 * 2026-09-13（Node 后端退役）：原先经 `./public-api-loader` 在**进程内**调用 Node 的
 * `/api/public-site-meta` 与 `/api/public-status` 路由（读公开状态投影）。Node 后端已删除，
 * `public-api-loader.ts` 随之删除——它的存在意义只是「进程内直达 Node 路由」。
 *
 * 现改为委托默认档（系统设置来源）。为何是委托而不是返回空：
 *  - 返回空会让调用方一律落到 `DEFAULT_SITE_TITLE` 与 `UTC`，**丢掉运维配好的站点标题与时区**；
 *  - 默认档的两个解析器本就是根布局的另一条分支（`src/app/[locale]/layout.tsx:33,91` 已直接
 *    引用它们），故本改法**不往依赖图里加任何新节点**；
 *  - 生产（导出的静态 UI）不受影响：导出壳已替换根布局，站点标题由 Go 壳在 `__CCH_BOOTSTRAP__`
 *    注入（见 `scripts/build-ui-export.mjs` 的 `EXPORT_ROOT_LAYOUT` 注释）。
 *
 * 上限（有意，登记在此）：不改为 HTTP 回取 Go 的 `/api/public-site-meta`——导出静态页无 SSR 可依，
 * 回取需要绝对地址与端口来源。若将来非导出的 SSR 部署重新成为正式形态，再补这条。
 */
export async function resolveSiteMetadataSource(): Promise<{
  siteTitle: string;
  siteDescription: string;
} | null> {
  return resolveDefaultSiteMetadataSource();
}

export async function resolveLayoutTimeZone(): Promise<string> {
  return resolveDefaultLayoutTimeZone();
}
