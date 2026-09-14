import { defaultLocale } from "./config";

/**
 * 退役语种的重定向壳（由 `scripts/build-ui-export.mjs` 写进 `out/<locale>/index.html`）。
 *
 * ## 为什么必须有这个文件
 *
 * Go 壳的 `resolve()` 对「已注册 locale 前缀 + 未命中路径」会返回 `<locale>/index.html`
 * （见 `go/internal/uiapp/uiapp.go` 的 resolve），而**注册集来自 Go 侧 `guard.SupportedLocales`、
 * 不与产物求交**。于是当前缀已退役、产物里又没有同名壳时，这个 URL 会取到零值 asset
 * —— 表现为**空 200**（既不是重定向，也不是 404）。写入一份壳，旧链接便被带到默认语言。
 *
 * ## 为什么不用服务端重定向
 *
 * 静态导出没有服务端 redirect，Go 壳只做静态文件服务（不识别重定向），故只能用客户端跳转；
 * 这与已有的根壳（`EXPORT_ROOT_PAGE`）是同一手法。
 *
 * ## 两条有意的语义（与根壳的区别）
 *
 * 1. **目标恒为默认语言**，不看 cookie 与浏览器语言：旧链接应当落到一个确定可服务的语种。
 *    根壳是「新访客入口」，按 cookie 选语言；这里是「失效链接的善后」，语义不同。
 * 2. **保留其余路径与查询/片段**：深层书签（如 `/ja/dashboard/logs?tab=1`）不会掉回首页。
 *
 * ## 为什么模板不带语种参数
 *
 * 该壳只会被 Go 壳在 `/<某个退役语种>/...` 上服务，故内联脚本只需**剥掉首段路径**，
 * 无须知道自己是哪个语种。于是三个语种共用同一份字节，也就不存在「清单与模板不同步」。
 *
 * 天花板（登记于此）：禁用 JS 的客户端只会看到一句提示与一个人工链接——与根壳同，
 * 本仓未投入无 JS 回退的功夫。
 */
export function retiredLocaleRedirectHtml(): string {
  const target = `/${defaultLocale}`;
  return `<!doctype html>
<html lang="${defaultLocale}">
  <head>
    <meta charset="utf-8" />
    <meta name="robots" content="noindex" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>${defaultLocale}</title>
    <script>
      (function () {
        var pathname = window.location.pathname;
        var nextSlash = pathname.indexOf("/", 1);
        var rest = nextSlash === -1 ? "" : pathname.slice(nextSlash);
        window.location.replace(
          ${JSON.stringify(target)} +
            (rest || "/dashboard") +
            window.location.search +
            window.location.hash
        );
      })();
    </script>
  </head>
  <body>
    <p>
      This language is no longer supported.
      <a href="${target}/dashboard/">Continue in ${defaultLocale}</a>.
    </p>
  </body>
</html>
`;
}
