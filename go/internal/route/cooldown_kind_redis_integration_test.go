package route

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件用**真 Redis** 钉住「冷却键按值分流」：同一个键、两种值、两种监控开关。
//
// 为何包内 stub 之外还要这一条：stub（slowrate_test.go 的 slowRateRedis）钉的是「分类逻辑对不对」，
// 而真库还要回答一个前提问题——值经真实 SETEX/GET 往返之后还是不是它。按值分流的全部依据就是
// 「读回来的值与写进去的逐字节相同」；这一条不成立时，分类逻辑再对也无从生效。
//
// 真库门控：未设 CCH_TEST_REDIS_URL 时跳过（CI 即跳过，与仓内同型用例一致）。
func TestCooldownKindAgainstRealRedis(t *testing.T) {
	client := integrationRedis(t)
	reader := NewSlowRateReader(SlowRateOptions{Redis: client})
	ctx := context.Background()

	const (
		sessionID = "sess_r5a_cooldown_kind"
		keyID     = int64(7)
	)
	// 三种组合各取一家，ID 取远离生产区间的值（键形制含 providerID，避免与真实键撞名）。
	monitoredSlow := baseProvider(9001, convert.ProviderClaude)
	monitoredSlow.SlowRateMonitorEnabled = true
	unmonitoredError := baseProvider(9002, convert.ProviderClaude) // 监控关闭（providers 该列 DB 默认）
	unmonitoredSlow := baseProvider(9003, convert.ProviderClaude)  // 监控关闭 + 低速标记

	write := func(providerID int64, value string) string {
		key := SlowRateCooldownKey(sessionID, keyID, providerID)
		if err := client.Set(ctx, key, value, time.Minute).Err(); err != nil {
			t.Fatalf("写冷却键失败（provider %d）: %v", providerID, err)
		}
		return key
	}
	keys := []string{
		write(monitoredSlow.ID, SlowRateCooldownMarker),
		write(unmonitoredError.ID, "42"),
		write(unmonitoredSlow.ID, SlowRateCooldownMarker),
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), keys...).Err() })

	got := reader.InCooldown(ctx, sessionID, keyID,
		[]Provider{monitoredSlow, unmonitoredError, unmonitoredSlow})

	if got[monitoredSlow.ID] != CooldownSlowRate {
		t.Errorf("监控开启+低速标记的成因 = %q，期望 %q", got[monitoredSlow.ID], CooldownSlowRate)
	}
	if got[unmonitoredError.ID] != CooldownProviderError {
		t.Errorf("监控关闭渠道的故障冷却成因 = %q，期望 %q（写侧不看监控开关，读侧也不能看）",
			got[unmonitoredError.ID], CooldownProviderError)
	}
	if _, cooling := got[unmonitoredSlow.ID]; cooling {
		t.Errorf("监控关闭渠道的低速冷却不该生效（关掉监控即停止该渠道的低速降级）")
	}
}
