"use client";

import { useEffect, useState } from "react";

interface CountdownResult {
  days: number;
  hours: number;
  minutes: number;
  seconds: number;
  totalSeconds: number;
  isExpired: boolean;
  shortFormatted: string; // 简短格式：如 "1d 2h 3m" 或 "2h 3m"
}

/**
 * 计算倒计时
 */
function calculateCountdown(targetDate: Date | null): CountdownResult {
  if (!targetDate) {
    return {
      days: 0,
      hours: 0,
      minutes: 0,
      seconds: 0,
      totalSeconds: 0,
      isExpired: true,
      shortFormatted: "0s",
    };
  }

  const now = Date.now();
  const target = new Date(targetDate).getTime();
  const diff = target - now;

  // 已过期
  if (diff <= 0) {
    return {
      days: 0,
      hours: 0,
      minutes: 0,
      seconds: 0,
      totalSeconds: 0,
      isExpired: true,
      shortFormatted: "0s",
    };
  }

  const totalSeconds = Math.floor(diff / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  // 格式化输出
  let shortFormatted = "";

  if (days > 0) {
    shortFormatted = `${days}d ${hours}h ${minutes}m`;
  } else if (hours > 0) {
    shortFormatted = `${hours}h ${minutes}m ${seconds}s`;
  } else if (minutes > 0) {
    shortFormatted = `${minutes}m ${seconds}s`;
  } else {
    shortFormatted = `${seconds}s`;
  }

  return {
    days,
    hours,
    minutes,
    seconds,
    totalSeconds,
    isExpired: false,
    shortFormatted,
  };
}

/**
 * 倒计时 Hook
 *
 * @param targetDate - 目标时间
 * @param enabled - 是否启用倒计时（默认 true）
 * @returns 倒计时对象
 *
 * @example
 * ```tsx
 * const countdown = useCountdown(resetTime);
 * return <div>{countdown.shortFormatted}</div>;
 * ```
 */
export function useCountdown(targetDate: Date | null, enabled: boolean = true): CountdownResult {
  const [mounted, setMounted] = useState(false);
  const [countdown, setCountdown] = useState<CountdownResult>(() => calculateCountdown(targetDate));

  // 挂载状态（避免 SSR 不一致）
  useEffect(() => {
    setMounted(true);
  }, []);

  // 倒计时更新
  useEffect(() => {
    if (!enabled || !mounted) return;

    // 初始计算
    setCountdown(calculateCountdown(targetDate));

    // 每秒更新
    const interval = setInterval(() => {
      setCountdown(calculateCountdown(targetDate));
    }, 1000);

    return () => clearInterval(interval);
  }, [targetDate, enabled, mounted]);

  // 未挂载时返回占位数据（避免 hydration 不一致）
  if (!mounted) {
    return {
      days: 0,
      hours: 0,
      minutes: 0,
      seconds: 0,
      totalSeconds: 0,
      isExpired: false,
      shortFormatted: "—",
    };
  }

  return countdown;
}
