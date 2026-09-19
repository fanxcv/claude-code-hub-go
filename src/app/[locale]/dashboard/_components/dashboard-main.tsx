"use client";

import type { ReactNode } from "react";
import { usePathname } from "@/i18n/routing";

interface DashboardMainProps {
  children: ReactNode;
}

export function DashboardMain({ children }: DashboardMainProps) {
  const pathname = usePathname();

  const normalizedPathname = pathname.endsWith("/") ? pathname.slice(0, -1) : pathname;

  // Pattern to match /dashboard/sessions/[id]/messages
  // The usePathname hook from next-intl/routing might return the path without locale prefix if configured that way,
  // or we just check for the suffix.
  // Let's be safe and check if it includes "/dashboard/sessions/" and ends with "/messages"
  const isSessionMessagesPage =
    normalizedPathname.includes("/dashboard/sessions/") && normalizedPathname.endsWith("/messages");

  if (isSessionMessagesPage) {
    return (
      <main className="h-[calc(var(--cch-viewport-height,100vh)-64px)] w-full overflow-hidden">
        {children}
      </main>
    );
  }

  // 使用记录页要「表格撑满可用高度」：main 是根容器（列 flex）里唯一可增高的一项，
  // 把「视口高减 header」的余量交给页面内容（页面链见 logs/page.tsx 与 usage-logs-view-virtualized）。
  // 链上取 grow/shrink-0（flex-basis:auto）而不是 flex-1（basis:0）：基线取自内容高度，
  // 只有真正多出来的空间才向下分配，小屏内容更高时不压缩链上任何一段，页面照常整体滚动。
  return (
    <main className="mx-auto flex w-full max-w-[100rem] grow shrink-0 flex-col px-6 py-8">
      {children}
    </main>
  );
}
