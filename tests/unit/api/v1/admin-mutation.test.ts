import { afterEach, describe, expect, test, vi } from "vitest";
import { clearCsrfTokenCache, postAdminMutation } from "@/lib/api-client/v1/fetcher";
import { CSRF_HEADER } from "@/lib/api/v1/_shared/constants";

/**
 * 根级 `/api/admin/*` 写请求的 CSRF 钉子。
 *
 * 为何单独钉：这些端点走 cookie 会话 + 写方法时，后端统一校验 `X-CCH-CSRF`
 * （go/internal/adminapi/auth.go:318-331），裸 fetch 不带该头即 403 auth.csrf_invalid
 * （生产实证：设置页改日志级别 403）。这里钉住「头名与取值来自同一份真源」以及
 * 「token 过期后能自愈」，避免再退回裸 fetch。
 */

type Call = { url: string; init: RequestInit };

function stubFetch(
  handler: (url: string, init: RequestInit | undefined, callIndex: number) => Response
) {
  const calls: Call[] = [];
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, init: init ?? {} });
    return handler(url, init, calls.length - 1);
  });
  vi.stubGlobal("fetch", mock);
  return { calls, mock };
}

function csrfHandler(extra?: (url: string) => Response | undefined) {
  return (url: string) => {
    const custom = extra?.(url);
    if (custom) return custom;
    if (url === "/api/v1/auth/csrf") {
      return Response.json({ csrfToken: "csrf-token-1" });
    }
    return Response.json({ ok: true });
  };
}

function headersOf(call: Call): Headers {
  return call.init.headers as Headers;
}

describe("postAdminMutation", () => {
  afterEach(() => {
    clearCsrfTokenCache();
    vi.unstubAllGlobals();
  });

  test("头名就是后端认的那一个（防止常量被改而无人察觉）", () => {
    expect(CSRF_HEADER).toBe("X-CCH-CSRF");
  });

  test("写请求带上 CSRF 头，且方法/凭据/正文与原裸 fetch 一致", async () => {
    const { calls } = stubFetch(csrfHandler());

    const body = { level: "debug" };
    const response = await postAdminMutation("/api/admin/log-level", body);

    expect(response.status).toBe(200);
    expect(calls.map((call) => call.url)).toEqual(["/api/v1/auth/csrf", "/api/admin/log-level"]);

    const mutation = calls[1];
    expect(mutation.init.method).toBe("POST");
    expect(mutation.init.credentials).toBe("include");
    expect(mutation.init.body).toBe(JSON.stringify(body));
    expect(headersOf(mutation).get("Content-Type")).toBe("application/json");
    expect(headersOf(mutation).get(CSRF_HEADER)).toBe("csrf-token-1");
  });

  test("同一次会话内复用 token（不每个请求打一次 csrf 端点）", async () => {
    const { calls, mock } = stubFetch(csrfHandler());

    await postAdminMutation("/api/admin/log-level", { level: "info" });
    await postAdminMutation("/api/admin/log-cleanup/manual", {
      beforeDate: "2026-01-01T00:00:00.000Z",
    });
    await postAdminMutation("/api/admin/system-config", { siteTitle: "t" });

    const csrfCalls = calls.filter((call) => call.url === "/api/v1/auth/csrf");
    expect(csrfCalls).toHaveLength(1);
    expect(mock).toHaveBeenCalledTimes(4);
    for (const call of calls.slice(1)) {
      expect(headersOf(call).get(CSRF_HEADER)).toBe("csrf-token-1");
    }
  });

  test("403 后清缓存：下一次调用重新取 token（否则会一直用被拒的 token 卡到 TTL 到期）", async () => {
    let attempt = 0;
    const { calls } = stubFetch(
      csrfHandler((url) => {
        if (url !== "/api/v1/auth/csrf") {
          attempt += 1;
          if (attempt === 1) {
            return Response.json(
              {
                status: 403,
                errorCode: "auth.csrf_invalid",
                detail: "CSRF token is missing or invalid.",
              },
              { status: 403, headers: { "Content-Type": "application/problem+json" } }
            );
          }
        }
        return undefined;
      })
    );

    const denied = await postAdminMutation("/api/admin/log-level", { level: "debug" });
    // 响应原样交给调用方：失败体是 `{error|errorCode, detail}`，由组件决定怎么提示。
    expect(denied.status).toBe(403);

    const retried = await postAdminMutation("/api/admin/log-level", { level: "debug" });
    expect(retried.status).toBe(200);

    const csrfCalls = calls.filter((call) => call.url === "/api/v1/auth/csrf");
    expect(csrfCalls).toHaveLength(2);
    expect(headersOf(calls[1]).get(CSRF_HEADER)).toBe("csrf-token-1");
  });

  test("csrf 端点不可用时仍发出请求（拿到 null 就不带头，由后端判定）", async () => {
    const { calls } = stubFetch(
      csrfHandler((url) =>
        url === "/api/v1/auth/csrf" ? new Response("nope", { status: 500 }) : undefined
      )
    );

    await postAdminMutation("/api/admin/log-level", { level: "info" });

    const mutation = calls[1];
    expect(mutation.url).toBe("/api/admin/log-level");
    expect(headersOf(mutation).has(CSRF_HEADER)).toBe(false);
  });
});
