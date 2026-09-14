"use client";

import { useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { useRouter } from "@/i18n/routing";

/**
 * 落地页：静态导出后不再有服务端 redirect，改由客户端路由跳转。
 *
 * 原实现是 `redirect({ href: "/dashboard", locale })`（服务端 307）；换成 i18n 路由的
 * `replace` 后语义一致：locale 前缀由 useRouter 按当前 locale 补齐。
 */
export default function Home() {
  const router = useRouter();

  useEffect(() => {
    router.replace("/dashboard");
  }, [router]);

  return <LoadingState className="p-6" />;
}
