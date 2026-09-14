package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件用**生产路径**（真实 handler + 真实 PG + 真实 Redis + 假上游）钉住会话观测接线：
// 密钥进、正文含客户端会话 id，出站之后 Redis 里必须出现会话三件套，且在途窗口里能看到
// 观测并发数为 1——这是「会话列表不再恒空、并发数不再恒 0」的可执行证据。
//
// 为什么必须走 handler 而不是直接调写侧：写侧的键形制已由 internal/session 的集成测试钉死，
// 这里要钉的是**时机与身份**——谁在什么时候把哪个身份写进去（尤其是 clientSessionId 与密钥
// 一起决定观测身份，写错了列表页会显示一个不存在的会话）。

// TestIntegrationHandlerWritesSessionObservation 是本文件的主用例。
func TestIntegrationHandlerWritesSessionObservation(t *testing.T) {
	rdb := dataPlaneIntegrationRedis(t)
	pools := integrationStore(t)
	ctx := context.Background()

	// 上游先卡住，让「在途」成为一个可观测的窗口：并发计数只有在途时才非 0。
	var enterOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5",`+
			`"content":[],"usage":{"input_tokens":7,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))
	assembly, err := NewStoreBacked(StoreOptions{
		Pools:  pools,
		Redis:  rdb,
		Logger: logx.New(nil),
		SessionArtifacts: session.SessionArtifactOptions{
			// 与 Node 出厂值一致：不存原文，落脱敏副本。
			StoreMessages: false,
			MaxBytes:      1 << 20,
		},
	})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}

	clientSessionID := fmt.Sprintf("sess_observe_%d", time.Now().UnixNano())
	identity := session.PublicSessionIdentity(clientSessionID, 0)
	// 普通会话 id 不经编码，身份就是它本身；这条断言保证下面的键名推导是正确的。
	if identity != clientSessionID {
		t.Fatalf("普通会话 id 不应被编码，收到 %q", identity)
	}
	cleanupObservation(t, rdb, clientSessionID, identity)

	// 正文带上客户端会话 id（metadata.user_id 的 JSON 形制），观测身份才不会每次随机。
	// 内层 user_id 必须是一个**JSON 字符串**，故用 json.Marshal 生成再 %q 包进正文，
	// 手写转义在此处最容易漏一层引号而静默退化成「随机会话 id」。
	metadataUserID, err := json.Marshal(map[string]string{
		"session_id": clientSessionID,
		"device_id":  "dev",
	})
	if err != nil {
		t.Fatalf("序列化 metadata.user_id 失败: %v", err)
	}
	body := fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":16,`+
		`"metadata":{"user_id":%q},`+
		`"system":"机密系统提示","messages":[{"role":"user","content":"机密问题"}]}`, string(metadataUserID))
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		assembly.Handler.ServeHTTP(recorder, request)
	}()

	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		close(release)
		<-done
		t.Fatal("请求未到达假上游（无法观测在途窗口）")
	}

	// 在途断言：观测并发计数必须为 1（列表页的「并发数」列就是读它）。
	reader := newObservationReader(t, rdb)
	counts, err := reader.ObservedConcurrentCounts(ctx, []string{identity})
	if err != nil {
		t.Fatalf("读观测并发数失败: %v", err)
	}
	if counts[identity] != 1 {
		t.Errorf("在途时观测并发数应为 1，收到 %d", counts[identity])
	}
	active, err := reader.ObservedActiveSessions(ctx)
	if err != nil {
		t.Fatalf("读观测集合失败: %v", err)
	}
	if !containsString(active, identity) {
		t.Errorf("在途时会话应在观测集合里（列表页据此显示活跃），收到 %v", active)
	}

	close(release)
	<-done
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}

	// 收尾断言：并发计数必须归零删键（否则每跑一次请求就泄漏一个计数键）。
	counts, err = reader.ObservedConcurrentCounts(ctx, []string{identity})
	if err != nil {
		t.Fatalf("读观测并发数失败: %v", err)
	}
	if counts[identity] != 0 {
		t.Errorf("收尾后观测并发数应为 0，收到 %d", counts[identity])
	}

	// 会话详情 Hash：字段、终态状态、TTL 与供应商都得对。
	info, err := rdb.HGetAll(ctx, session.InfoKey(identity)).Result()
	if err != nil {
		t.Fatalf("读会话 info 失败: %v", err)
	}
	if len(info) == 0 {
		t.Fatalf("请求结束后应存在 %s", session.InfoKey(identity))
	}
	if info["keyName"] == "" || info["userId"] == "" || info["keyId"] == "" {
		t.Errorf("info 缺少可显示的身份字段: %v", info)
	}
	if info["model"] != "claude-sonnet-4-5" || info["apiType"] != "chat" {
		t.Errorf("info 的 model/apiType 应为 claude-sonnet-4-5/chat，收到 %q/%q", info["model"], info["apiType"])
	}
	if info["status"] != "completed" {
		t.Errorf("2xx 终态的 status 应为 completed，收到 %q", info["status"])
	}
	if info["providerId"] != fmt.Sprintf("%d", provisioned.providerID) {
		t.Errorf("info.providerId 应为本次选中的 %d，收到 %q", provisioned.providerID, info["providerId"])
	}
	ttl, err := rdb.TTL(ctx, session.InfoKey(identity)).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > 300*time.Second {
		t.Errorf("info 的 TTL 应为 (0, 300s]，收到 %v", ttl)
	}

	// 物理会话进全局活跃 ZSET（与 Node 共用同一组键）。
	if _, err := rdb.ZScore(ctx, session.ActiveSessionsGlobalKey(), clientSessionID).Result(); err != nil {
		t.Errorf("全局活跃 ZSET 应含物理会话 id: %v", err)
	}

	// 请求工件：高并发模式开启时**一件都不落**（Node 的 shouldPersistSessionDebugArtifacts）；
	// 关闭时才落脱敏副本。两条分支都按库里真实设置断言，而不是假设哪一种。
	settings, err := pools.FindSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读系统设置失败: %v", err)
	}
	artifactExists, err := rdb.Exists(ctx, session.RequestBodyKey(clientSessionID, 1)).Result()
	if err != nil {
		t.Fatalf("查请求正文工件失败: %v", err)
	}
	sequence := 1
	if !settings.EnableHighConcurrencyMode {
		if artifactExists != 1 {
			t.Error("未开高并发模式时应落请求正文工件")
		}
		stored, err := rdb.Get(ctx, session.RequestBodyKey(clientSessionID, sequence)).Result()
		if err != nil {
			t.Fatalf("读请求正文工件失败: %v", err)
		}
		if strings.Contains(stored, "机密问题") || strings.Contains(stored, "机密系统提示") {
			t.Errorf("STORE_SESSION_MESSAGES=false 不得留下正文原文，收到 %q", stored)
		}
	} else if artifactExists != 0 {
		t.Error("高并发模式下不得落请求工件（每流内存的主要放大器）")
	}
}

// newObservationReader 组装只读的会话观测读面（复用生产实现，不另写一套键名）。
func newObservationReader(t *testing.T, rdb redis.UniversalClient) *session.Binder {
	t.Helper()
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本注册表失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return session.NewBinder(client)
}

// cleanupObservation 只删本用例写的观测键（唯一会话 id，不影响其它用例）。
func cleanupObservation(t *testing.T, rdb redis.UniversalClient, sessionID string, identity string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = rdb.ZRem(ctx, session.ObservedGlobalActiveSessionsKey(), identity).Result()
		_, _ = rdb.ZRem(ctx, session.ActiveSessionsGlobalKey(), sessionID).Result()
		keys := []string{
			session.InfoKey(identity),
			session.ObservedConcurrentCountKey(identity),
			session.ConcurrentCountKey(sessionID),
			session.RequestBodyKey(sessionID, 1),
			session.MessagesSequenceKey(sessionID, 1),
			session.LastSeenKey(sessionID),
		}
		_ = rdb.Del(ctx, keys...).Err()
	})
}
