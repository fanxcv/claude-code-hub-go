package adminapi

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// 本文件钉住对拍发现的 D-TZ 缺陷在**端点层**的可见结果：
//
// 生产库里 system_settings.timezone 多为 NULL，Node 侧 resolveSystemTimezone 会退到 env TZ，
// 而 zod 声明了默认 Asia/Shanghai（env.schema.ts:179）；Go 侧过去在调用点裸读
// os.Getenv("TZ")，未设置时得空串即落 UTC。两侧因此同库同刻给出不同的时区，
// 「今日」类统计随之按不同日界落桶（对拍实测 today 计数 Go 4 / Node 903）。
//
// 这条比 store 层的钉子更靠外：它走完整路由，钉的是「对拍台会看到的那个值」。

// TestSystemIntegrationTimezoneEndpointFallsBackToEnvDefault 钉住库列 NULL 时的端点应答。
func TestSystemIntegrationTimezoneEndpointFallsBackToEnvDefault(t *testing.T) {
	pools := meOpenPools(t)
	principal := Principal{UserID: 1, KeyID: 1, IsAdmin: true}
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterSystemRoutes(router, Deps{
		Guard:    principalGuard{principal: principal},
		Problems: NewProblems(nil),
		Store:    pools,
	})

	raw, err := pools.AdminSystemTimezone(context.Background())
	if err != nil {
		t.Fatalf("读库时区失败: %v", err)
	}
	if raw != nil && *raw != "" {
		t.Skipf("库里的 system_settings.timezone 已有值 %q，本用例只覆盖 NULL 场景", *raw)
	}

	previous, hadTZ := os.LookupEnv("TZ")
	t.Cleanup(func() {
		if hadTZ {
			_ = os.Setenv("TZ", previous)
		} else {
			_ = os.Unsetenv("TZ")
		}
	})

	if err := os.Unsetenv("TZ"); err != nil {
		t.Fatalf("移除 TZ 失败: %v", err)
	}
	status, body := meGet(t, router, "/system/timezone")
	if status != http.StatusOK {
		t.Fatalf("timezone 状态应为 200，实际 %d：%+v", status, body)
	}
	if body["timeZone"] != "Asia/Shanghai" {
		t.Fatalf("库列 NULL 且 TZ 未设置时应答应为默认 Asia/Shanghai（与 zod 默认一致），实际 %v",
			body["timeZone"])
	}

	// TZ 显式设置时以环境为准（zod 对「已设置」同样取原值）。
	if err := os.Setenv("TZ", "Asia/Tokyo"); err != nil {
		t.Fatalf("设置 TZ 失败: %v", err)
	}
	status, body = meGet(t, router, "/system/timezone")
	if status != http.StatusOK {
		t.Fatalf("timezone 状态应为 200，实际 %d：%+v", status, body)
	}
	if body["timeZone"] != "Asia/Tokyo" {
		t.Fatalf("TZ=Asia/Tokyo 时应答应为 Asia/Tokyo，实际 %v", body["timeZone"])
	}
}
