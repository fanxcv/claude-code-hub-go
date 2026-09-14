import type { NextConfig } from "next";
import createNextIntlPlugin from "next-intl/plugin";

// Create next-intl plugin with i18n request configuration
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

// UI 静态导出开关：CCH_UI_EXPORT=1 时产出 output:"export"（默认 standalone 管线一字不变）。
// 完整流程见 scripts/build-ui-export.mjs（导出前会临时移走 /api /v1 /v1beta 与中间件，构建后还原）。
const uiExport = process.env.CCH_UI_EXPORT === "1";

const nextConfig: NextConfig = {
  output: "standalone",

  // 转译 ESM 模块（@lobehub/icons 需要）
  transpilePackages: ["@lobehub/icons"],

  // 排除服务端专用包（避免打包到客户端）
  // bull 和相关依赖只在服务端使用，包含 Node.js 原生模块
  // postgres 和 drizzle-orm 包含 Node.js 原生模块（net, tls, crypto, stream, perf_hooks）
  serverExternalPackages: [
    "bull",
    "bullmq",
    "@bull-board/api",
    "@bull-board/express",
    "ioredis",
    "postgres",
    "drizzle-orm",
  ],

  // 强制包含 undici 和 fetch-socks 到 standalone 输出
  // Next.js 依赖追踪无法正确追踪动态导入和类型导入的传递依赖
  // 参考: https://nextjs.org/docs/app/api-reference/config/next-config-js/output
  outputFileTracingIncludes: {
    "/**": [
      "./node_modules/undici/**/*",
      "./node_modules/fetch-socks/**/*",
      // 自定义 Node 服务器（server.js）只用到 `ws` 与 next 的入口；
      // 让 Next 的依赖追踪决定从 next 包里收纳什么文件，避免把 next 整个
      // node_modules 都拖进 standalone 产物（约数十 MB）。仅显式追加：
      //  - ws：standalone 默认追踪基于 import 静态分析，server.js 是 CJS
      //    根入口，未被 Next 编译，必须手工列出。
      //  - next/dist：自定义 server 通过 require("next") 进入；保留 dist
      //    子树确保 programmatic API 可用。
      "./node_modules/ws/**/*",
      "./node_modules/next/dist/**/*",
      "./node_modules/next/package.json",
    ],
  },

  // 文件上传大小限制（用于数据库备份导入）
  // Next.js 15 通过 serverActions.bodySizeLimit 统一控制
  experimental: {
    serverActions: {
      bodySizeLimit: "500mb",
    },
    proxyClientMaxBodySize: "100mb",
  },
};

if (uiExport) {
  // 静态导出专用键：locale 路由产物落成 <locale>/<route>/index.html，配合 Go 壳的 index.html 回退。
  nextConfig.output = "export";
  nextConfig.trailingSlash = true;
  nextConfig.images = { unoptimized: true };
  // 导出是**打包步骤**：此仓以 `"use server"` 标注 repository/actions（导出时必须移出，否则
  // Next 报 "Server Actions are not supported with static export"），而客户端组件以
  // `import type` 从它们取类型。故导出构建不做类型检查——类型门禁由 `bun run typecheck` 独立把守。
  nextConfig.typescript = { ignoreBuildErrors: true };
  // 服务端专属键在导出下无意义（standalone 依赖追踪/外部包豁免/server actions 上限），避免干扰。
  for (const key of [
    "serverExternalPackages",
    "outputFileTracingIncludes",
    "experimental",
  ] as const) {
    delete nextConfig[key];
  }
}

// Wrap the Next.js config with next-intl plugin
export default withNextIntl(nextConfig);
