// isDevelopment 从 env-flags 取，不经 env.schema：后者为校验整份环境变量依赖 zod，
// 而 logger 是**全站共享**模块——经它引入 zod 会让 66/72 个路由的共享 chunk 里多出整个校验库
// （实测 376 KiB 解码 / 84 KiB br），而这里只需要读一个布尔。语义两侧一致（见 env-flags 的说明）。
//
// 并发说明：另一路 lane 独立做了同一处修复，其新模块名为 config/is-development；合并时取
// env-flags（此模块已在 main、注释更全），is-development 若被引入则一并收敛到本模块，免得留两份同义模块。
import { isDevelopment } from "./config/env-flags";

/**
 * 日志级别类型
 */
export type LogLevel = "fatal" | "error" | "warn" | "info" | "debug" | "trace";

type LoggerWrapper = {
  fatal: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  error: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  warn: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  info: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  debug: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  trace: (arg1: unknown, arg2?: unknown, ...args: unknown[]) => void;
  level: string;
};

const levelPriority: Record<LogLevel, number> = {
  trace: 10,
  debug: 20,
  info: 30,
  warn: 40,
  error: 50,
  fatal: 60,
};

/**
 * 获取初始日志级别
 * - 优先使用环境变量 LOG_LEVEL
 * - 开发环境默认 debug
 * - 生产环境默认 info
 */
function getInitialLogLevel(): LogLevel {
  const envLevel = process.env.LOG_LEVEL?.toLowerCase();
  const validLevels: LogLevel[] = ["fatal", "error", "warn", "info", "debug", "trace"];

  if (envLevel && validLevels.includes(envLevel as LogLevel)) {
    return envLevel as LogLevel;
  }

  // 向后兼容：如果设置了 DEBUG_MODE，使用 debug 级别
  if (process.env.DEBUG_MODE === "true") {
    return "debug";
  }

  return isDevelopment() ? "debug" : "info";
}

function isValidLevel(level: string): level is LogLevel {
  return level in levelPriority;
}

function createConsoleLogger(initialLevel: LogLevel): LoggerWrapper {
  let currentLevel: LogLevel = initialLevel;

  const shouldLog = (level: LogLevel) => levelPriority[level] >= levelPriority[currentLevel];
  const wrap = (method: (...args: unknown[]) => void, level: LogLevel) => {
    return (arg1: unknown, arg2?: unknown, ...args: unknown[]) => {
      if (!shouldLog(level)) return;
      method(arg1, arg2, ...args);
    };
  };

  return {
    fatal: wrap(console.error, "fatal"),
    error: wrap(console.error, "error"),
    warn: wrap(console.warn, "warn"),
    info: wrap(console.info, "info"),
    debug: wrap(console.debug, "debug"),
    trace: wrap(console.trace, "trace"),
    get level() {
      return currentLevel;
    },
    set level(newLevel: string) {
      if (isValidLevel(newLevel)) {
        currentLevel = newLevel;
      }
    },
  };
}

// 只有 console 一种实现：原 pino 路径已随 Node 时代退役移除——它由动态 import
// 载入且前置 `typeof window !== "undefined"` 即返回，而静态导出产物只在浏览器里跑，
// 那条分支永远不会走到有意义的实现。
export const logger: LoggerWrapper = createConsoleLogger(getInitialLogLevel());

/**
 * 运行时动态调整日志级别
 * @param newLevel 新的日志级别
 */
export function setLogLevel(newLevel: LogLevel): void {
  logger.level = newLevel;
  logger.info(`日志级别已调整为: ${newLevel}`);
}

/**
 * 获取当前日志级别
 */
export function getLogLevel(): string {
  return logger.level;
}
