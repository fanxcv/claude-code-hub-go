package slowrate

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// TestCooldownWriteRequiresKeyIdentity 钉住 W4：写侧与读侧前置条件一致——无 key 身份不写冷却键。
//
// 为什么必须一致：读侧（route.SlowRateReader.InCooldown）在 keyID==0 时直接返回空集，压根不会
// 去查键；写侧若仍按 keyID=0 写下键，那把键永远读不到，只留下一个 60 秒后自灭的死键。
//
// 用 recoveryTestRedis：本判据须在 CI 默认跑法（不设 CCH_TEST_REDIS_URL）下也真的能拦，
// 否则把 writeCooldown 的 KeyID 校验放宽都不会转红。
func TestCooldownWriteRequiresKeyIdentity(t *testing.T) {
	h := newRecoveryHarnessOn(t, recoveryTestRedis(t), 9106, 10)
	ctx := context.Background()

	// keyID=0：无 key 身份，不写。
	noKey := h.slowFactsFor(601)
	noKey.SessionID = "sess_lane2_no_key"
	noKey.KeyID = 0
	for index := int64(1); index <= 3; index++ {
		facts := noKey
		facts.RequestID = 600 + index
		h.recorder.Record(ctx, facts)
	}
	if h.exists(session.ProviderCooldownKey(noKey.SessionID, 0, h.provider)) {
		t.Fatal("keyID=0 时不该写冷却键（读侧不会查它，只会留一个死键）")
	}

	// 对照：同一模型组合、带 key 身份时照写——证明上面的跳过是因为 keyID，不是别的门。
	withKey := h.slowFactsFor(701)
	withKey.SessionID = "sess_lane2_with_key"
	withKey.KeyID = 7
	for index := int64(1); index <= 3; index++ {
		facts := withKey
		facts.RequestID = 700 + index
		h.recorder.Record(ctx, facts)
	}
	if !h.exists(session.ProviderCooldownKey(withKey.SessionID, withKey.KeyID, h.provider)) {
		t.Fatal("带 key 身份的慢样本应写冷却键（对照）")
	}
}
