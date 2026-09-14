# i18n（界面多语言）

本目录是界面侧的多语言装配（`next-intl`）。**先读这条分工**：界面只做**静态导出**，
导出产物里**没有服务端、也没有 Next middleware**；语种前缀与退役语种的旧 URL 由导出脚本
生成的页面壳兜住，登录态由 Go 侧校验。

## 支持语种

| 语种 | 状态 |
| --- | --- |
| `zh-CN` | **默认** |
| `en` | 支持 |
| `zh-TW`、`ru`、`ja` | **已退役**：词表已删、路由不再生成；旧 URL 由重定向壳接住 |

`locales` 与 `retiredLocales` **必须互斥**（导出脚本会断言，重复即报错）。两者语义不同：

- `locales` 参与路由与词表装配；
- `retiredLocales` 只解决「旧 URL 不失效」，既不参与路由也不参与词表。

## 文件

| 文件 | 职责 |
| --- | --- |
| [config.ts](config.ts) | 语种清单、默认语种、标签、cookie 名、退役语种清单 |
| [routing.ts](routing.ts) | `defineRouting`（`localePrefix: "always"`）与类型安全导航（`Link`/`redirect`/`useRouter`/`usePathname`） |
| [pathname.ts](pathname.ts) | 语种判定与「导航用 pathname」归一（登录后回跳的安全兜底也用它） |
| [request.ts](request.ts) | 构建期的 `getRequestConfig`：语种归一 → 动态导入 `messages/<locale>` → 解析系统时区 |
| [client-messages.ts](client-messages.ts) | `getClientMessages(locale)`：给客户端 Provider（[../components/i18n-provider.tsx](../components/i18n-provider.tsx)）按语种装配词表 |
| [retired-locale.ts](retired-locale.ts) | `retiredLocaleRedirectHtml()`：退役前缀的重定向壳 HTML（导出脚本写入每个退役前缀目录） |

## 语种如何决定（静态导出形态）

1. 所有路由都带前缀（`localePrefix: "always"`），例如 `/zh-CN/dashboard`；
2. 无前缀路径（`/`、`/dashboard`）由 **Go 侧页面壳**接住并落到默认语种——见
   [../../go/internal/uiapp/uiapp.go](../../go/internal/uiapp/uiapp.go) 的 `DefaultLocale`
   （与 [config.ts](config.ts) 的 `defaultLocale` 同值，改一处必须改另一处）；
3. 退役前缀（`/ru/...`）由导出脚本生成的**重定向壳**接住，跳到默认语种的对应页；
4. **鉴权不在本层**：静态产物里没有中间件，未登录/无权限由 Go 侧判定。

## 词表真源

- 词表在仓库根的 `messages/zh-CN/` 与 `messages/en/`（按命名空间分文件，`settings/` 再分子目录）；
- **两份词表的键必须一致**：改一份必须同步另一份；
- 用户可见文案一律走词表，**不得硬编码**；
- 审计命令：`bun run i18n:audit-messages-no-emoji`（词表禁 emoji）、
  `bun run i18n:audit-placeholders`（占位符一致性）。

## 用法

```tsx
// 客户端组件
"use client";
import { useTranslations } from "next-intl";
import { Link } from "@/i18n/routing";

export function Card() {
  const t = useTranslations("namespace");
  return (
    <div>
      <h1>{t("title")}</h1>
      <Link href="/dashboard">{t("goToDashboard")}</Link>
    </div>
  );
}
```

```tsx
// 服务端组件：只在**构建期**执行（静态导出），不要在里做请求时才能决定的事
import { getTranslations } from "next-intl/server";

export default async function Page() {
  const t = await getTranslations("namespace");
  return <h1>{t("title")}</h1>;
}
```

## 相关命令

```bash
bun run dev        # 仅界面调试（tsgo 预检 + next dev），**不含后端**
bun run build      # 导出静态产物并压缩嵌入 Go（scripts/build-ui-export.mjs + build-ui-embed.mjs）
bun run typecheck
bun run test
```

## 改动语种清单时要同步的地方

新增或退役语种，至少四处要一起改（漏一处会出现「切到不存在的语种」或「导出仍出旧语种目录」）：

1. [config.ts](config.ts) 的 `locales` / `retiredLocales`（互斥）；
2. `messages/` 下的词表目录与文件；
3. 导出脚本 `scripts/build-ui-export.mjs` 读取的语种清单与它生成的重定向壳；
4. Go 侧 [../../go/internal/uiapp/uiapp.go](../../go/internal/uiapp/uiapp.go) 的默认语种与语种注册。
