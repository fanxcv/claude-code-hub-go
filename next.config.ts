import type { NextConfig } from "next";
import createNextIntlPlugin from "next-intl/plugin";

// Create next-intl plugin with i18n request configuration
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

// UI 静态导出开关：CCH_UI_EXPORT=1 时产出 output:"export"。
// 完整流程见 scripts/build-ui-export.mjs（导出前会临时移出服务端绑定页面，构建后还原）。
const uiExport = process.env.CCH_UI_EXPORT === "1";

const nextConfig: NextConfig = {
  // 转译 ESM 模块（@lobehub/icons 需要）
  transpilePackages: ["@lobehub/icons"],
};

if (uiExport) {
  // 静态导出专用键：locale 路由产物落成 <locale>/<route>/index.html，配合 Go 壳的 index.html 回退。
  nextConfig.output = "export";
  nextConfig.trailingSlash = true;
  nextConfig.images = { unoptimized: true };
  // 导出是**打包步骤**：客户端组件以 `import type` 从服务端模块取类型，导出构建不做
  // 类型检查——类型门禁由 `bun run typecheck` 独立把守。
  nextConfig.typescript = { ignoreBuildErrors: true };
}

// Wrap the Next.js config with next-intl plugin
export default withNextIntl(nextConfig);
