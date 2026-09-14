import { afterEach, describe, expect, test, vi } from "vitest";
import {
  DEFAULT_NODE_ENV,
  DEFAULT_TIMEZONE,
  envTimezone,
  isDevelopment,
} from "@/lib/config/env-flags";
import { EnvSchema } from "@/lib/config/env.schema";

/**
 * `env-flags` 是一份**刻意重复**的取值面：它不依赖 zod，好让 logger / 时区解析这类全站共享模块
 * 不必把整个校验库拖进客户端 chunk。重复就意味着可能漂移，故这里把两侧钉在一起——
 * 改 `env.schema.ts` 的默认值而不同步本文件，这里就红。
 */
describe("env-flags 与 EnvSchema 的默认值绑定", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  test("DEFAULT_NODE_ENV 等于 EnvSchema.NODE_ENV 的默认值", () => {
    expect(EnvSchema.shape.NODE_ENV.parse(undefined)).toBe(DEFAULT_NODE_ENV);
  });

  test("DEFAULT_TIMEZONE 等于 EnvSchema.TZ 的默认值", () => {
    expect(EnvSchema.shape.TZ.parse(undefined)).toBe(DEFAULT_TIMEZONE);
  });
});

describe("envTimezone", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  test("未设时落 schema 同值默认；空串原样返回；已设时原样返回", () => {
    vi.stubEnv("TZ", undefined);
    expect(envTimezone()).toBe(DEFAULT_TIMEZONE);

    // 空串在 schema 里是合法字符串（z.string() 接受 ""），调用方按真值判断后落到 UTC 兜底，
    // 故这里也必须原样返回空串，不能替它填默认值——否则「env TZ 为空」会变成「用上海时区」。
    vi.stubEnv("TZ", "");
    expect(envTimezone()).toBe("");

    vi.stubEnv("TZ", "America/New_York");
    expect(envTimezone()).toBe("America/New_York");
  });
});

describe("isDevelopment", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  test("production 为假、development 为真、未设时按 schema 默认值判定", () => {
    vi.stubEnv("NODE_ENV", "production");
    expect(isDevelopment()).toBe(false);

    vi.stubEnv("NODE_ENV", "development");
    expect(isDevelopment()).toBe(true);

    vi.stubEnv("NODE_ENV", undefined);
    expect(isDevelopment()).toBe(DEFAULT_NODE_ENV === "development");
  });
});
