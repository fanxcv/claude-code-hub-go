package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usersreset"
)

// 本文件是两条统计重置路由的端到端用例（真 PG + 真 Redis）：202 + Location → 200 的往返、
// 404 的两种来路（用户不存在 / 作业不存在）。
//
// 键名在这里按字面量拼：它们是**跨进程契约**（Go 与 Node 共用），usersreset 包不导出它们，
// 而测试需要精确清理自己造的状态（不 FLUSHDB——这个 Redis 实例是压测与其它 lane 共用的）。

const usersResetStatusPrefix = "cch:user-statistics-reset:status:"
const usersResetActivePrefix = "cch:user-statistics-reset:active:"
const usersResetFixed5hPrefix = "cch:user-statistics-reset:fixed5h:"
const usersResetPendingKey = "cch:user-statistics-reset:go:pending"

// usersResetIntegration 是端到端的装配。
type usersResetIntegration struct {
	router *Router
	pools  *store.Pools
	redis  redis.UniversalClient
	queue  *usersreset.Queue
}

// newUsersResetIntegration 装配真依赖；缺门控变量即跳过。
func newUsersResetIntegration(t *testing.T) *usersResetIntegration {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("Redis 不可用，跳过 Redis 集成测试: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	queue := usersreset.NewQueue(usersreset.QueueOptions{
		Status: usersreset.NewStatusStore(client),
		Pools:  pools,
	})
	guard := &recordingGuard{}
	deps := Deps{Guard: guard, Store: pools, Problems: NewProblems(nil), UsersReset: queue}
	router := New(Options{Deps: deps})
	RegisterUsersResetRoutes(router, deps)
	return &usersResetIntegration{router: router, pools: pools, redis: client, queue: queue}
}

// newUser 建一个专属用户并登记清理。
func (integration *usersResetIntegration) newUser(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	name := fmt.Sprintf("go-admin-users-reset-it-%d-%d", time.Now().UnixNano(), os.Getpid())
	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, name).Scan(&userID); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool, err := integration.pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID
}

// cleanupJob 清掉一次作业的三个键与队列成员。
func (integration *usersResetIntegration) cleanupJob(t *testing.T, userID int64, resetID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_ = integration.redis.Del(ctx,
			usersResetStatusPrefix+resetID,
			usersResetActivePrefix+fmt.Sprint(userID),
			usersResetFixed5hPrefix+resetID,
		).Err()
		members, err := integration.redis.ZRange(ctx, usersResetPendingKey, 0, -1).Result()
		if err != nil {
			return
		}
		for _, member := range members {
			if strings.Contains(member, resetID) {
				_ = integration.redis.ZRem(ctx, usersResetPendingKey, member).Err()
			}
		}
	})
}

// TestUsersResetRoutesEndToEnd 钉住「排一次 → 查一次」的完整往返与两个 404 分支。
func TestUsersResetRoutesEndToEnd(t *testing.T) {
	integration := newUsersResetIntegration(t)
	userID := integration.newUser(t)

	// 用户不存在：Node 的 action 先查用户，查不到即 404（错误码 NOT_FOUND）。
	missing := httptest.NewRecorder()
	integration.router.ServeHTTP(missing, httptest.NewRequest(
		http.MethodPost, fmt.Sprintf("%s/users/%d/statistics:reset", MountPrefix, userID+1_000_000_000), nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("用户不存在应得 404，实际 %d（%s）", missing.Code, missing.Body.String())
	}

	created := httptest.NewRecorder()
	integration.router.ServeHTTP(created, httptest.NewRequest(
		http.MethodPost, fmt.Sprintf("%s/users/%d/statistics:reset", MountPrefix, userID), nil))
	if created.Code != http.StatusAccepted {
		t.Fatalf("入队应得 202，实际 %d（%s）", created.Code, created.Body.String())
	}
	var body struct {
		ResetID                string  `json:"resetId"`
		UserID                 int64   `json:"userId"`
		Status                 string  `json:"status"`
		RequestedAt            string  `json:"requestedAt"`
		StartedAt              *string `json:"startedAt"`
		CompletedAt            *string `json:"completedAt"`
		DeletedMessageRequests int64   `json:"deletedMessageRequests"`
		DeletedUsageLedger     int64   `json:"deletedUsageLedger"`
		ErrorCode              *string `json:"errorCode"`
		Fixed5hKeyIDs          any     `json:"fixed5hKeyIds"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 202 正文失败: %v（%s）", err, created.Body.String())
	}
	if body.Status != "queued" {
		t.Fatalf("入队后状态应为 queued，实际 %s", body.Status)
	}
	if body.UserID != userID {
		t.Fatalf("正文里的用户不符：%d", body.UserID)
	}
	if len(body.ResetID) != 36 {
		t.Fatalf("作业 id 应是 uuid，实际 %q", body.ResetID)
	}
	// 公开响应不得含对内字段（Node 的 schema 不含 fixed5hKeyIds）。
	if body.Fixed5hKeyIDs != nil || strings.Contains(created.Body.String(), "fixed5hKeyIds") {
		t.Fatalf("公开响应不得含对内字段：%s", created.Body.String())
	}
	integration.cleanupJob(t, userID, body.ResetID)

	// Location 与 Node 逐字一致（硬编码 /api/v1 前缀）。
	location := created.Header().Get("Location")
	wantLocation := fmt.Sprintf("%s/users/%d/statistics-resets/%s", MountPrefix, userID, body.ResetID)
	if location != wantLocation {
		t.Fatalf("Location 不符：得到 %q，期望 %q", location, wantLocation)
	}

	// 状态查询：200 + 同一条作业。
	statusRecorder := httptest.NewRecorder()
	integration.router.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodGet, location, nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("查状态应得 200，实际 %d（%s）", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusBody struct {
		ResetID string `json:"resetId"`
		Status  string `json:"status"`
		UserID  int64  `json:"userId"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("解析 200 正文失败: %v", err)
	}
	if statusBody.ResetID != body.ResetID || statusBody.Status != "queued" || statusBody.UserID != userID {
		t.Fatalf("状态正文不符：%+v", statusBody)
	}

	// 作业不存在：404 + 该路由特有的错误码。
	unknownRecorder := httptest.NewRecorder()
	integration.router.ServeHTTP(unknownRecorder, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("%s/users/%d/statistics-resets/0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4", MountPrefix, userID),
		nil,
	))
	if unknownRecorder.Code != http.StatusNotFound {
		t.Fatalf("未知作业应得 404，实际 %d", unknownRecorder.Code)
	}
	var problemBody struct {
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal(unknownRecorder.Body.Bytes(), &problemBody); err != nil {
		t.Fatalf("解析 404 正文失败: %v", err)
	}
	if problemBody.ErrorCode != "user.statistics_reset_not_found" {
		t.Fatalf("404 错误码不符：%s", problemBody.ErrorCode)
	}

	// 跨用户查询：另一个用户拿不到这条作业（同样 404，不泄露存在性）。
	otherUserID := integration.newUser(t)
	crossRecorder := httptest.NewRecorder()
	integration.router.ServeHTTP(crossRecorder, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("%s/users/%d/statistics-resets/%s", MountPrefix, otherUserID, body.ResetID),
		nil,
	))
	if crossRecorder.Code != http.StatusNotFound {
		t.Fatalf("跨用户查询应得 404，实际 %d（%s）", crossRecorder.Code, crossRecorder.Body.String())
	}
}
