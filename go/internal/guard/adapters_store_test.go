package guard

import (
	"context"
	"testing"
	"time"
)

// TestAuthStoreInvalidateClearsBothCaches 钉住失效广播一次清两份缓存，且清完之后的下一次解析
// 重新读行源（而不是回放进失效前的旧行）。
//
// 为什么用户缓存也要清：管理面没有用户域通道，用户被禁用/删除/改名单时唯一能到达这里的信号就是
// api_keys 广播（adminapi 的 InvalidateUserAuth）。用户行同时喂给鉴权判定（is_enabled）与身份判定
// （allowed/blocked clients 与 models），留在缓存里就是一个能在 TTL 内放行的旧事实。
func TestAuthStoreInvalidateClearsBothCaches(t *testing.T) {
	ctx := context.Background()
	loads := 0
	authStore := newAuthStore(authStoreOptions{
		keyTTL:  time.Minute,
		userTTL: time.Minute,
		logger:  defaultLogger,
		rows: func(context.Context, string) ([]string, error) {
			loads++
			return []string{`{"key_id":7,"key_name":"k","key_is_enabled":true,"user_id":9,` +
				`"user_name":"u","user_is_enabled":true}`}, nil
		},
	})

	if _, err := authStore.ResolveAPIKey(ctx, "sk-x"); err != nil {
		t.Fatalf("首次解析应命中注入的行源：%v", err)
	}
	if got := authStore.keys.Size(); got != 1 {
		t.Fatalf("首次解析后密钥缓存应 1 条，实际 %d", got)
	}
	if _, ok := authStore.users.Get(9); !ok {
		t.Fatal("密钥命中应顺带填用户缓存")
	}

	authStore.Invalidate()

	if got := authStore.keys.Size(); got != 0 {
		t.Fatalf("失效后密钥缓存应为空，实际 %d 条", got)
	}
	if _, ok := authStore.users.Get(9); ok {
		t.Fatal("失效后用户缓存不应再命中：用户禁用/改名只靠这一条广播到达")
	}

	if _, err := authStore.ResolveAPIKey(ctx, "sk-x"); err != nil {
		t.Fatalf("失效后重解析失败：%v", err)
	}
	if loads != 2 {
		t.Fatalf("失效后应重新装载行源，装载次数应为 2，实际 %d", loads)
	}
}
