package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是隔离态块（provider_slow_logs.go 的 slowLogsAttachQuarantine）的**无库**用例。
//
// 为什么与同目录那组真库用例分开：那组要过可见性门（真 providers 表），本组只钉
// 「读侧观测 -> 响应形状」这一跳——它不碰库，故在无 CCH_TEST_DSN 的环境（CI 正是这样跑的）
// 也真的会执行。真库那组只留一条（两条读面的先后顺序），因为唯一必须穿过 handler 才能钉的。
//
// 读侧本身（枚举口径、准入阶梯、活窗计数）的钉子全在 route 侧的 slowrate_observe_test.go。

// slowLogsAttachQuarantineResponse 打一发装配并返回响应体（不经过路由与可见性门）。
func slowLogsAttachQuarantineResponse(t *testing.T, states SlowRateStateReader) slowLogsResponse {
	t.Helper()
	deps := Deps{Logger: logx.New(nil), SlowRateStates: states}
	response := slowLogsResponse{ProviderID: 7, Events: []slowLogsEvent{}}
	slowLogsAttachQuarantine(&response, httptest.NewRequest(http.MethodGet, "/", nil), deps,
		&store.AdminProvider{ID: 7})
	return response
}

// TestSlowLogsQuarantineAttachReflectsBothStates 钉住「state 在 / 不在」两种情形下响应结构都正确。
//
// 右侧那条是生产 2026-09-22 的实测形态（状态键不存在、滑窗为空、只有干净计数）——它必须看得见，
// 否则「机制在跑但从未触发隔离」与「监控根本没开」在运维眼里仍然没有区别。
func TestSlowLogsQuarantineAttachReflectsBothStates(t *testing.T) {
	states := &slowLogsFakeStates{observations: []route.SlowRateStateObservation{
		{
			ModelKey:          "m-slow",
			StateExists:       true,
			Quarantined:       true,
			Penalty:           20,
			CleanStreak:       4,
			AdmissionPermille: 100,
			SampleLiveCount:   5,
			BaselineUsable:    true,
			ProbeLeaseHeld:    true,
			ProbeLeaseTTL:     12 * time.Second,
		},
		{
			// 未隔离的组合：读面（route.ObserveStates）给的是 1000（全放）——本层只透传，
			// 「未隔离即 1000」的真钉子在 route 侧的 slowrate_observe_test.go。
			ModelKey:          "m-clean",
			CleanStreak:       2,
			AdmissionPermille: 1000,
			BaselineUsable:    true,
		},
	}}
	response := slowLogsAttachQuarantineResponse(t, states)
	if response.Quarantine == nil {
		t.Fatalf("quarantine 为 null，期望有内容")
	}
	if response.Quarantine.UnavailableReason != nil {
		t.Errorf("unavailableReason = %q，期望 null", *response.Quarantine.UnavailableReason)
	}
	byModel := map[string]slowLogsQuarantineCombination{}
	for _, combination := range response.Quarantine.Combinations {
		byModel[combination.ModelKey] = combination
	}
	if len(byModel) != 2 {
		t.Fatalf("组合数 = %d，期望 2（实得 %v）", len(byModel), byModel)
	}

	slow := byModel["m-slow"]
	if !slow.StateExists || !slow.Quarantined {
		t.Errorf("m-slow 的 stateExists/quarantined = %v/%v，期望 true/true", slow.StateExists, slow.Quarantined)
	}
	if slow.Penalty != 20 || slow.CleanStreak != 4 || slow.SampleLiveCount != 5 {
		t.Errorf("m-slow 的 penalty/cleanStreak/sampleLiveCount = %d/%d/%d，期望 20/4/5",
			slow.Penalty, slow.CleanStreak, slow.SampleLiveCount)
	}
	if slow.AdmissionPermille != 100 {
		t.Errorf("m-slow 的 admissionPermille = %d，期望 100", slow.AdmissionPermille)
	}
	if !slow.BaselineUsable || !slow.ProbeLeaseHeld || slow.ProbeLeaseTTLMillis != 12_000 {
		t.Errorf("m-slow 的 baselineUsable/租约 = %v/%v/%d ms，期望 true/true/12000",
			slow.BaselineUsable, slow.ProbeLeaseHeld, slow.ProbeLeaseTTLMillis)
	}

	clean := byModel["m-clean"]
	if clean.StateExists || clean.Quarantined {
		t.Errorf("m-clean 的 stateExists/quarantined = %v/%v，期望 false/false", clean.StateExists, clean.Quarantined)
	}
	if clean.CleanStreak != 2 || !clean.BaselineUsable {
		t.Errorf("m-clean 的 cleanStreak/baselineUsable = %d/%v，期望 2/true", clean.CleanStreak, clean.BaselineUsable)
	}
	if clean.AdmissionPermille != 1000 {
		t.Errorf("m-clean 的 admissionPermille = %d，期望 1000（未隔离即全放）", clean.AdmissionPermille)
	}
	// 无租约键时读侧的 TTL 是负数（Redis PTTL 语义），对外统一成 -1。
	if clean.ProbeLeaseHeld || clean.ProbeLeaseTTLMillis != -1 {
		t.Errorf("m-clean 的租约 = %v/%d ms，期望 false/-1", clean.ProbeLeaseHeld, clean.ProbeLeaseTTLMillis)
	}

	// 传下去的必须是**渠道行**而不是光一个 id：读侧据行上的四参数算活窗下界与档位，
	// 只传 id 会让管理面回退状态里的旧参数，与数据面读数分叉。
	if len(states.providers) != 1 {
		t.Fatalf("读面被调 %d 次，期望 1", len(states.providers))
	}
	if states.providers[0].ID != 7 || !states.providers[0].SlowRateMonitorEnabled {
		t.Errorf("候选 = %+v，期望 id=7 且按已开启报（slowRateCandidates 的构造口径）", states.providers[0])
	}
}

// TestSlowLogsQuarantineAttachUnwired 钉住未装配时该块保持 null（不是空对象、不是 []）。
//
// 为什么不能用空对象冒充：「读面没装」与「没有任何组合」必须可区分——前者是能力缺失，
// 后者是正常读数（与 slowRate / diverts 三态同一纪律）。
func TestSlowLogsQuarantineAttachUnwired(t *testing.T) {
	response := slowLogsAttachQuarantineResponse(t, nil)
	if response.Quarantine != nil {
		t.Fatalf("未装配时 quarantine = %+v，期望 null", response.Quarantine)
	}
	// 序列化后必须是字面 null（前端据 null 整段不显示）。
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := string(decoded["quarantine"]); got != "null" {
		t.Errorf("未装配时 quarantine = %s，期望 null", got)
	}
}

// TestSlowLogsQuarantineCombinationKeys 钉住组合行的**字段名**（新接口面，将来前端要靠它读）。
//
// 为什么不能只断 Go 结构体字段：结构体字段改了名字，序列化出去的名字也会变，而消费方是按
// JSON 键读的——只断结构体等于放任字段名自由漂移。故这里断序列化后的**字面键集**。
func TestSlowLogsQuarantineCombinationKeys(t *testing.T) {
	response := slowLogsAttachQuarantineResponse(t, &slowLogsFakeStates{
		observations: []route.SlowRateStateObservation{{ModelKey: "m1", AdmissionPermille: 1000}},
	})
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded struct {
		Quarantine struct {
			Combinations []map[string]json.RawMessage `json:"combinations"`
		} `json:"quarantine"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(decoded.Quarantine.Combinations) != 1 {
		t.Fatalf("组合数 = %d，期望 1", len(decoded.Quarantine.Combinations))
	}
	want := []string{
		"modelKey", "stateExists", "quarantined", "penalty", "cleanStreak",
		"admissionPermille", "sampleLiveCount", "baselineUsable",
		"probeLeaseHeld", "probeLeaseTtlMillis",
	}
	row := decoded.Quarantine.Combinations[0]
	for _, key := range want {
		if _, ok := row[key]; !ok {
			t.Errorf("组合行缺字段 %q", key)
		}
	}
	if len(row) != len(want) {
		t.Errorf("组合行字段数 = %d，期望 %d（实得 %v）", len(row), len(want), row)
	}
}

// TestSlowLogsQuarantineAttachReadFailure 钉住读失败给「空表 + 明确原因」，而不是把端点打成 5xx。
//
// 与事件流同纪律：读不到是排障时最需要知道的，用 500 会让运维以为整个入口坏了；
// 而「读不到」与「没有组合」也必须能区分。
func TestSlowLogsQuarantineAttachReadFailure(t *testing.T) {
	response := slowLogsAttachQuarantineResponse(t,
		&slowLogsFakeStates{err: errors.New("redis: connection refused")})
	if response.Quarantine == nil {
		t.Fatalf("quarantine 为 null，期望带原因的对象")
	}
	if response.Quarantine.UnavailableReason == nil ||
		*response.Quarantine.UnavailableReason != "redis_unavailable" {
		t.Errorf("unavailableReason = %v，期望 redis_unavailable", response.Quarantine.UnavailableReason)
	}
	if response.Quarantine.Combinations == nil {
		t.Errorf("combinations 为 null，期望空数组（前端不必为 null 与 [] 各写一条分支）")
	}
	if len(response.Quarantine.Combinations) != 0 {
		t.Errorf("combinations 长度 = %d，期望 0", len(response.Quarantine.Combinations))
	}
}

// TestSlowLogsQuarantineKeepsExistingFields 钉住**只增不改**：响应字段集恰好是既有五个 + quarantine。
//
// 这是对外接口，故不只断「新字段在」，还断「既有字段一个不少、也没有多出别的」——
// 只断前者的话，误删一个既有字段不会被任何用例发现。
func TestSlowLogsQuarantineKeepsExistingFields(t *testing.T) {
	response := slowLogsAttachQuarantineResponse(t, &slowLogsFakeStates{})
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"providerId", "window", "events", "diverts", "unavailableReason", "quarantine"}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("响应缺字段 %q", key)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("响应字段数 = %d，期望 %d（实得 %v）", len(decoded), len(want), decoded)
	}
	// 空读面的组合是空数组而不是 null：前端不必为两种空值各写一条分支。
	if got := string(decoded["quarantine"]); got == "null" {
		t.Errorf("装配了读面却给 null")
	}
	if response.Quarantine.Combinations == nil {
		t.Errorf("combinations 为 null，期望空数组")
	}
}
