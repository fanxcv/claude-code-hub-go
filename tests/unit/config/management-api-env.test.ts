import { afterEach, describe, expect, test, vi } from "vitest";

async function loadEnv() {
  vi.resetModules();
  return import("@/lib/config/env.schema");
}

describe("management API env flags", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  test("uses safe defaults", async () => {
    vi.stubEnv("ENABLE_API_KEY_ADMIN_ACCESS", undefined);
    vi.stubEnv("CSRF_SECRET", undefined);

    const { getEnvConfig, isApiKeyAdminAccessEnabled } = await loadEnv();
    const env = getEnvConfig();

    expect(isApiKeyAdminAccessEnabled()).toBe(false);
    expect(env.CSRF_SECRET).toBeUndefined();
  });

  test("keeps optional env fields unset", async () => {
    for (const key of [
      "DSN",
      "DB_POOL_MAX",
      "DB_POOL_IDLE_TIMEOUT",
      "DB_POOL_CONNECT_TIMEOUT",
      "MESSAGE_REQUEST_ASYNC_FLUSH_INTERVAL_MS",
      "MESSAGE_REQUEST_ASYNC_BATCH_SIZE",
      "MESSAGE_REQUEST_ASYNC_MAX_PENDING",
      "ADMIN_TOKEN",
      "CSRF_SECRET",
    ]) {
      vi.stubEnv(key, undefined);
    }

    const { getEnvConfig } = await loadEnv();
    const env = getEnvConfig();

    expect(env.DSN).toBeUndefined();
    expect(env.DB_POOL_MAX).toBeUndefined();
    expect(env.MESSAGE_REQUEST_ASYNC_BATCH_SIZE).toBeUndefined();
    expect(env.ADMIN_TOKEN).toBeUndefined();
    expect(env.CSRF_SECRET).toBeUndefined();
  });

  test("parses explicit boolean strings without z.coerce.boolean pitfalls", async () => {
    vi.stubEnv("ENABLE_API_KEY_ADMIN_ACCESS", "true");
    vi.stubEnv("ENABLE_SECURE_COOKIES", "false");

    const { getEnvConfig, isApiKeyAdminAccessEnabled } = await loadEnv();

    expect(isApiKeyAdminAccessEnabled()).toBe(true);
    expect(getEnvConfig().ENABLE_SECURE_COOKIES).toBe(false);
  });

  test("parses zero as false", async () => {
    vi.stubEnv("ENABLE_API_KEY_ADMIN_ACCESS", "0");

    const { isApiKeyAdminAccessEnabled } = await loadEnv();

    expect(isApiKeyAdminAccessEnabled()).toBe(false);
  });

  test("parses dedicated CSRF secret", async () => {
    vi.stubEnv("CSRF_SECRET", "dedicated-csrf-secret");

    const { getEnvConfig } = await loadEnv();

    expect(getEnvConfig().CSRF_SECRET).toBe("dedicated-csrf-secret");
  });

  test("treats blank and placeholder CSRF secrets as unset", async () => {
    vi.stubEnv("CSRF_SECRET", "change-me");

    let env = await loadEnv();
    expect(env.getEnvConfig().CSRF_SECRET).toBeUndefined();

    vi.stubEnv("CSRF_SECRET", "");
    env = await loadEnv();
    expect(env.getEnvConfig().CSRF_SECRET).toBeUndefined();
  });

  test("rejects short CSRF secrets", async () => {
    vi.stubEnv("CSRF_SECRET", "short");

    const { getEnvConfig } = await loadEnv();

    expect(() => getEnvConfig()).toThrow("CSRF_SECRET");
  });
});
