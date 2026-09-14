package session

import (
	"context"
	"strings"
	"testing"
)

// 本文件钉住**响应侧工件**的写读闭环：数据面按下述顺序写入 → 详情面读出来。
//
// 为什么必须成对钉：写侧与读侧的键名/形状分叉不会让任何请求失败——写进 A、读 B 的后果是
// 详情页**恒空**，而两侧各自的单测都会通过（各测各的键）。故这里按「写入 → 读出」串起来，
// 并且**从生产入口进**（Binder 的 Store* / Read* 方法），不手写 Redis 键。

// TestIntegrationResponseArtifactsRoundTrip 覆盖响应侧六类工件的写读闭环。
func TestIntegrationResponseArtifactsRoundTrip(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()
	sequence := 1

	options := SessionArtifactOptions{StoreMessages: false, StoreResponseBody: true}
	if err := binder.StoreSessionRequestOwner(ctx, sessionID, sequence, testKeyID); err != nil {
		t.Fatalf("写所有者键失败: %v", err)
	}

	// 1. 响应正文：STORE_SESSION_MESSAGES=false ⇒ JSON 正文落脱敏副本，非 JSON 原样落。
	if err := binder.StoreSessionResponse(ctx, sessionID, sequence, testKeyID,
		[]byte(`{"content":[{"type":"text","text":"机密回答"}]}`), options); err != nil {
		t.Fatalf("写响应正文失败: %v", err)
	}
	body, found := binder.SessionResponseBody(ctx, sessionID, sequence)
	if !found {
		t.Fatal("响应正文写后读不到——写读键名已分叉")
	}
	if strings.Contains(body, "机密回答") {
		t.Fatalf("STORE_SESSION_MESSAGES=false 时正文未脱敏：%s", body)
	}
	if !strings.Contains(body, redactedMarker) {
		t.Fatalf("脱敏副本应带 %s 标记：%s", redactedMarker, body)
	}

	// 2. 双侧头：形状必须与读侧一致（键名不同，值形态相同）。
	requestHeaders := map[string]string{"Anthropic-Version": "2023-06-01"}
	if err := binder.StoreSessionRequestHeaders(ctx, sessionID, sequence, requestHeaders); err != nil {
		t.Fatalf("写请求头失败: %v", err)
	}
	responseHeaders := map[string]string{"Content-Type": "application/json"}
	if err := binder.StoreSessionResponseHeaders(
		ctx, sessionID, sequence, testKeyID, responseHeaders); err != nil {
		t.Fatalf("写响应头失败: %v", err)
	}
	if got := binder.SessionRequestHeaders(ctx, sessionID, sequence); got["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("请求头读回不符：%v", got)
	}
	if got := binder.SessionResponseHeaders(ctx, sessionID, sequence); got["Content-Type"] != "application/json" {
		t.Fatalf("响应头读回不符：%v", got)
	}

	// 3. 上游元信息：URL 必须过 sanitizeUrl（敏感查询参数换 [REDACTED]）。
	if err := binder.StoreSessionUpstreamRequestMeta(ctx, sessionID, sequence,
		SessionUpstreamRequestMeta{
			URL:    "https://api.example.com/v1/messages?api_key=sk-secret&x=1",
			Method: "POST",
		}); err != nil {
		t.Fatalf("写上游请求元信息失败: %v", err)
	}
	if err := binder.StoreSessionUpstreamResponseMeta(ctx, sessionID, sequence, testKeyID,
		SessionUpstreamResponseMeta{
			URL:        "https://api.example.com/v1/messages?token=t0ken",
			StatusCode: 200,
		}); err != nil {
		t.Fatalf("写上游响应元信息失败: %v", err)
	}
	reqMeta := binder.ReadSessionUpstreamRequestMeta(ctx, sessionID, sequence)
	if reqMeta == nil {
		t.Fatal("上游请求元信息读不到")
	}
	if strings.Contains(reqMeta.URL, "sk-secret") {
		t.Fatalf("上游 URL 未脱敏：%s", reqMeta.URL)
	}
	if reqMeta.Method != "POST" {
		t.Fatalf("method 读回不符：%s", reqMeta.Method)
	}
	resMeta := binder.ReadSessionUpstreamResponseMeta(ctx, sessionID, sequence)
	if resMeta == nil || resMeta.StatusCode != 200 {
		t.Fatalf("上游响应元信息读回不符：%+v", resMeta)
	}
	if strings.Contains(resMeta.URL, "t0ken") {
		t.Fatalf("上游响应 URL 未脱敏：%s", resMeta.URL)
	}

	// 4. 相位快照：四份（request/response × before/after）各自独立键。
	snapshots := []struct{ kind, phase string }{
		{"request", "before"}, {"request", "after"},
		{"response", "before"}, {"response", "after"},
	}
	for _, entry := range snapshots {
		snapshot := SessionDetailPhaseSnapshot{Meta: SessionDetailPhaseMeta{}}
		switch entry.kind {
		case "request":
			snapshot.Body = map[string]any{"messages": []any{map[string]any{"role": "user"}}}
			snapshot.Messages = []any{map[string]any{"role": "user"}}
			snapshot.HasMessages = true
			snapshot.Headers = map[string]string{"X-Test": entry.phase}
		case "response":
			snapshot.Body = "data: {}\n\n"
			snapshot.Headers = map[string]string{"X-Test": entry.phase}
			status := 200
			snapshot.Meta.StatusCode = &status
		}
		url := "https://api.example.com/v1/messages"
		if entry.kind == "request" {
			snapshot.Meta.UpstreamURL = &url
			method := "POST"
			snapshot.Meta.Method = &method
		} else {
			snapshot.Meta.UpstreamURL = &url
		}
		fields := binder.StoreSessionPhaseSnapshot(ctx, sessionID, sequence, testKeyID,
			entry.kind, entry.phase, snapshot, PhaseSnapshotOptions{StoreMessages: false})
		if len(fields) == 0 {
			t.Fatalf("%s/%s 快照一个字段都没写进去", entry.kind, entry.phase)
		}
		read := binder.ReadSessionPhaseSnapshot(ctx, sessionID, sequence, entry.kind, entry.phase)
		if read == nil {
			t.Fatalf("%s/%s 快照写后读不到", entry.kind, entry.phase)
		}
		if read.Headers["X-Test"] != entry.phase {
			t.Fatalf("%s/%s 头读回不符：%v", entry.kind, entry.phase, read.Headers)
		}
	}

	// 写侧落盘后必须**同时**刷新所有者围栏（Node 的响应侧写入器都 refresh）。
	if !binder.IsSessionRequestOwnedByKey(ctx, sessionID, sequence, testKeyID) {
		t.Fatal("响应侧写入后所有者围栏失效")
	}
	if binder.IsSessionRequestOwnedByKey(ctx, sessionID, sequence, testKeyID+1) {
		t.Fatal("别的 keyId 不该通过所有者围栏")
	}
}

// TestIntegrationPhaseSnapshotOversizeDeletesKey 单字段超限时**删键**而不是跳过。
//
// 为什么必须删：同一序号可能被重写（重试/故障转移）。若超限时只是「不写」，读侧会把这一次的
// 元信息配上**上一次**的正文——比没有正文更坏。
func TestIntegrationPhaseSnapshotOversizeDeletesKey(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()
	sequence := 1

	// 先写一份小快照（模拟上一次的正文）。
	small := SessionDetailPhaseSnapshot{
		Body:    "小正文",
		Headers: map[string]string{"X-Test": "small"},
		Meta:    SessionDetailPhaseMeta{},
	}
	if fields := binder.StoreSessionPhaseSnapshot(ctx, sessionID, sequence, testKeyID,
		"response", "after", small, PhaseSnapshotOptions{StoreMessages: true}); len(fields) == 0 {
		t.Fatal("小快照应写入成功")
	}

	// 再写一份超过单字段上限的大快照：正文键必须被**删掉**。
	big := SessionDetailPhaseSnapshot{
		Body:    strings.Repeat("x", 4096),
		Headers: map[string]string{"X-Test": "big"},
		Meta:    SessionDetailPhaseMeta{},
	}
	binder.StoreSessionPhaseSnapshot(ctx, sessionID, sequence, testKeyID,
		"response", "after", big, PhaseSnapshotOptions{StoreMessages: true, MaxBytes: 1024})

	read := binder.ReadSessionPhaseSnapshot(ctx, sessionID, sequence, "response", "after")
	if read == nil {
		t.Fatal("头字段仍应存在（只有超限的正文键被删）")
	}
	if read.HasBody {
		t.Fatalf("超限正文应被删键，读回却是：%.40v", read.Body)
	}
	if read.Headers["X-Test"] != "big" {
		t.Fatalf("头字段应被刷新成新值：%v", read.Headers)
	}
}

// TestIntegrationResponseBodyCaptureBoundedWindow 有界头尾捕获的落盘形态。
func TestIntegrationResponseBodyCaptureBoundedWindow(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	capture := NewResponseCapture(32)
	capture.Write([]byte(strings.Repeat("H", 100)))
	capture.Write([]byte(strings.Repeat("T", 100)))
	payload := capture.Bytes()

	if err := binder.StoreSessionResponse(ctx, sessionID, 1, testKeyID, payload,
		SessionArtifactOptions{StoreMessages: true, StoreResponseBody: true}); err != nil {
		t.Fatalf("写窗口正文失败: %v", err)
	}
	body, found := binder.SessionResponseBody(ctx, sessionID, 1)
	if !found {
		t.Fatal("窗口正文写后读不到")
	}
	if !strings.Contains(body, "[TRUNCATED: 136 bytes omitted]") {
		t.Fatalf("截断标记与省略量不符：%q", body)
	}
	if !strings.HasPrefix(body, strings.Repeat("H", 32)) ||
		!strings.HasSuffix(body, strings.Repeat("T", 32)) {
		t.Fatalf("头尾窗口不符：%q", body)
	}
}

// TestIntegrationResponseBodySwitchSkipsWrite 总开关关闭时**一个键都不写**。
func TestIntegrationResponseBodySwitchSkipsWrite(t *testing.T) {
	rdb := testRedis(t)
	binder := newTestBinder(t, rdb)
	sessionID := uniqueSessionID(t)
	cleanupSessionKeys(t, rdb, sessionID, testKeyID)
	ctx := context.Background()

	if err := binder.StoreSessionResponse(ctx, sessionID, 1, testKeyID, []byte("正文"),
		SessionArtifactOptions{StoreMessages: true, StoreResponseBody: false}); err != nil {
		t.Fatalf("开关关闭时写应静默跳过而不是报错: %v", err)
	}
	if _, found := binder.SessionResponseBody(ctx, sessionID, 1); found {
		t.Fatal("STORE_SESSION_RESPONSE_BODY=false 时不应落下任何正文")
	}
}
