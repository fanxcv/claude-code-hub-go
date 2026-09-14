import type { Metadata } from "next";
import { getTranslations, setRequestLocale } from "next-intl/server";
import { UsageDocChrome } from "./_components/usage-doc-chrome";

type UsageDocParams = { locale: string };

/**
 * 文档页面布局：提供文档段落的 title/description，并把外壳交给客户端组件。
 *
 * 为什么本文件仍是服务端组件而外壳是客户端：静态导出没有服务端会话，头部与登录态判定
 * 必须在客户端于首帧完成（会话取自 Go 壳注入的 `__CCH_BOOTSTRAP__`）；但 `generateMetadata`
 * 只能由服务端组件导出，去掉它会让文档页失去 title/description（Node 版经
 * `getTranslations` 设置过，导出时会被写进各页 `<head>`）。
 *
 * 本文件**不得**引用 `@/lib/auth`、`@/repository/`、`@/actions/` 或 `next/headers|cookies`：
 * `scripts/build-ui-export.mjs` 的 `isServerBound` 一旦命中就会把整个 layout 移出导出，
 * 连同它渲染的头部一起从产物里消失（这正是本轮要修的问题）。
 * `next-intl/server` 不在该判定内，导出壳自己也用它。
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<UsageDocParams> | UsageDocParams;
}): Promise<Metadata> {
  const { locale } = await params;
  const t = await getTranslations({ locale, namespace: "usage" });
  return {
    title: t("pageTitle"),
    description: t("pageDescription"),
  };
}

export default async function UsageDocLayout({
  children,
  params,
}: {
  children: React.ReactNode;
  params: Promise<UsageDocParams> | UsageDocParams;
}) {
  const { locale } = await params;
  // 静态渲染下 next-intl 要求逐段声明 locale，否则 getTranslations 退化为动态渲染。
  setRequestLocale(locale);

  return <UsageDocChrome>{children}</UsageDocChrome>;
}
