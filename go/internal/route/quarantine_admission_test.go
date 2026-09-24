package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件补「低速隔离的**闸门放行**档」这一面的钉子：quarantine_test.go 的既有三条亲和钉子只盖
// 「闸门拒」的家（准入比例 0），而放行档（连续干净样本把比例抬到 10%/30%）的家不进
// quarantineExcluded，是生产 1081750 的实际形态——慢渠道被亲和粘住，健康候选全部旁落。
//
// 反证：把 select.go 的 underSlowQuarantine 换回 quarantineExcluded ⇒ 下面 1、2 两条转红。

// admittingSessionID 造一个「本次闸门放行」的会话身份。
//
// 准入判据是 (渠道, key, 会话, 10 秒时间桶) 的稳定哈希，逐请求不可控；测试须按同一哈希挑会话
// 才能落到放行分支。用扫描而非硬编码：哈希实现变动时用例仍能自证落点。
func admittingSessionID(t *testing.T, providerID, keyID int64, permille int) string {
	t.Helper()
	now := time.UnixMilli(slowRateTestNowMS)
	for index := 0; index < 10000; index++ {
		sessionID := "admitting-" + strconv.Itoa(index)
		if quarantineAdmits(permille, providerID, keyID, sessionID, now) {
			return sessionID
		}
	}
	t.Fatalf("找不到能通过准入闸门的会话身份（provider=%d key=%d permille=%d）", providerID, keyID, permille)
	return ""
}

// TestQuarantineAdmittedSessionBindingDoesNotShortCircuit 钉住：闸门放行时，会话绑定也不得短路。
//
// 反证：把 select.go 会话绑定闸的判据换回 `!quarantineExcluded[bound.ID]` ⇒ 本用例选中 1（session_reuse），红。
func TestQuarantineAdmittedSessionBindingDoesNotShortCircuit(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, QuarantineAdmissionTenFrom)
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))
	sessionID := admittingSessionID(t, 1, 7, quarantineAdmissionTenPermille)

	result, err := selector.Select(context.Background(), Request{
		Model:          "m1",
		SessionID:      sessionID,
		KeyID:          7,
		SessionBinding: &SessionBindingSnapshot{ProviderID: 1, Generation: "3"},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Fatalf("闸门放行时仍被会话绑定短路为 session_reuse：选中 = %v", result.Provider)
	}
	if result.Provider == nil || result.Provider.ID == 1 {
		t.Fatalf("选中 = %v，期望非 1（被隔离的家不得被粘住，即便闸门放行）", result.Provider)
	}
	// 隔离属临时原因，须留痕；闸门放行的家不在过滤留痕里，故这条同时钉住留痕保真。
	if result.SessionBindingBypass != SessionBindingBypassTransient {
		t.Errorf("绑定 bypass = %v，期望 transient", result.SessionBindingBypass)
	}
}

// TestQuarantineAdmittedPrefixAffinityDoesNotShortCircuit 是同款对前缀亲和路径的钉子。
//
// 反证：把 select.go 前缀亲和闸的判据换回 `!quarantineExcluded[...]` ⇒ 本用例记 prefix_affinity 并选中 1，红。
func TestQuarantineAdmittedPrefixAffinityDoesNotShortCircuit(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, QuarantineAdmissionTenFrom)
	selector := newFailOpenSelector(t, client, slowRateProvider(1), slowRateProvider(2))
	sessionID := admittingSessionID(t, 1, 7, quarantineAdmissionTenPermille)

	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatal("夹具正文应能指纹化")
	}
	result, err := selector.Select(context.Background(), Request{
		Model:           "m1",
		Format:          convert.FormatClaude,
		KeyID:           7,
		SessionID:       sessionID,
		SessionIdentity: SessionIdentityRecovered,
		AffinityBody:    body,
		AffinityLookup: &AffinityLookup{
			Hint:       &AffinityHint{ProviderID: 1, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
			IdentityFP: "idfp",
			Generation: "v3:gen",
		},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodPrefixAffinity {
		t.Fatalf("闸门放行时仍被前缀亲和短路为 prefix_affinity：选中 = %v", result.Provider)
	}
	if result.Provider == nil || result.Provider.ID == 1 {
		t.Fatalf("选中 = %v，期望非 1（被隔离的家不得被前缀亲和粘住，即便闸门放行）", result.Provider)
	}
}

// TestQuarantineAdmittedSoleCandidateStillSelected 钉住：唯一候选带隔离态且闸门放行时仍被选中。
//
// 反证：把放行档的家也当真排除（underSlowQuarantine 被误接到过滤阶段）⇒ 唯一候选落进软信号
// 回退甚至 no_provider；本用例钉住「放行档照常参与正常选路，不新增 503」。
func TestQuarantineAdmittedSoleCandidateStillSelected(t *testing.T) {
	client := quarantineRedis(t, 1, "m1", true, QuarantineAdmissionTenFrom)
	selector := newFailOpenSelector(t, client, slowRateProvider(1))
	sessionID := admittingSessionID(t, 1, 7, quarantineAdmissionTenPermille)

	result, err := selector.Select(context.Background(), Request{Model: "m1", SessionID: sessionID, KeyID: 7})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("选中 = %v，期望 1（闸门放行的唯一候选照常参与选路，不得新增 503）", result.Provider)
	}
	if result.Method != MethodWeightedRandom {
		t.Errorf("selectionMethod = %q，期望 %q", result.Method, MethodWeightedRandom)
	}
}
