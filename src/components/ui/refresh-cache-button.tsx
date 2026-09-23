"use client";

import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

type RefreshCacheOutcome =
  | { ok: true; data?: { stats: { totalCount: number } } }
  | { ok: false; error: string };

interface RefreshCacheButtonProps {
  stats: { totalCount: number } | null;
  label: string;
  title: string;
  className: string;
  refresh: () => Promise<RefreshCacheOutcome>;
  successMessage: (count: number) => string;
  failureMessage: string;
  onRefreshed?: () => void;
}

export function RefreshCacheButton({
  stats,
  label,
  title,
  className,
  refresh,
  successMessage,
  failureMessage,
  onRefreshed,
}: RefreshCacheButtonProps) {
  const [isRefreshing, setIsRefreshing] = useState(false);

  const handleRefresh = async () => {
    setIsRefreshing(true);

    try {
      const result = await refresh();

      if (!result.ok) {
        toast.error(result.error);
        return;
      }

      if (!result.data) throw new Error("refresh cache response is missing stats");

      toast.success(successMessage(result.data.stats.totalCount));
      onRefreshed?.();
    } catch {
      toast.error(failureMessage);
    } finally {
      setIsRefreshing(false);
    }
  };

  return (
    <Button
      variant="outline"
      onClick={handleRefresh}
      disabled={isRefreshing}
      className={className}
      title={title}
    >
      <RefreshCw className={cn("mr-2 h-4 w-4", isRefreshing && "animate-spin")} />
      {label}
      {stats && <span className="ml-2 text-xs text-muted-foreground">({stats.totalCount})</span>}
    </Button>
  );
}
