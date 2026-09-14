package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// unreachableRedisClient 造一个「脚本调用层已装配、但 Redis 不可达」的连接。
//
// 用途：把「输入预检」与「契约解析」两段纯逻辑与真实 Redis 解耦。构造期不建连，因此这些
// 用例在无 Redis 的机器上也能跑；任何真的走到网络的调用都会立即失败，反而暴露测试意图跑偏。
func unreachableRedisClient(t *testing.T) *ratelimit.Client {
	t.Helper()
	registry, err := ratelimit.Load()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: 0})
	t.Cleanup(func() { _ = rdb.Close() })
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return client
}

// TestBinderRequiresClient 钉住「未装配脚本调用层」的可判别错误。
func TestBinderRequiresClient(t *testing.T) {
	ctx := context.Background()
	binder := NewBinder(nil)
	if binder.Ready() {
		t.Fatal("nil 调用层的 Binder 不应 Ready")
	}
	if ReadinessOfNilBinder() {
		t.Fatal("nil Binder 不应 Ready")
	}

	calls := []struct {
		name string
		call func() (BindingResult, error)
	}{
		{"ReadOrReconcile", func() (BindingResult, error) {
			return binder.ReadOrReconcile(ctx, "sess_abc", 42, 300)
		}},
		{"CompareAndSet", func() (BindingResult, error) {
			return binder.CompareAndSet(ctx, "sess_abc", 42, "gen-1", 1, 300)
		}},
		{"Touch", func() (BindingResult, error) {
			return binder.Touch(ctx, "sess_abc", 42, "gen-1", 1, 300)
		}},
		{"Clear", func() (BindingResult, error) {
			return binder.Clear(ctx, "sess_abc", 42, "gen-1", 1, 0, 0, 300)
		}},
		{"Terminate", func() (BindingResult, error) {
			return binder.Terminate(ctx, "sess_abc", 42, 1, 300)
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); !errors.Is(err, ErrNilClient) {
				t.Fatalf("应返回 ErrNilClient: got=%v", err)
			}
		})
	}

	leaseCalls := []struct {
		name string
		call func() error
	}{
		{"AcquireLease", func() error {
			_, err := binder.AcquireLease(ctx, "sess_abc", 42, "", 300)
			return err
		}},
		{"RenewLease", func() error {
			_, err := binder.RenewLease(ctx, "sess_abc", 42, "token", 300)
			return err
		}},
		{"ReleaseLease", func() error {
			_, err := binder.ReleaseLease(ctx, "sess_abc", 42, "token")
			return err
		}},
	}
	for _, tc := range leaseCalls {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrNilClient) {
				t.Fatalf("应返回 ErrNilClient: got=%v", err)
			}
		})
	}

	// 跟踪与展示类方法在未装配时静默跳过（Node 侧同样只在 ready 时写）。
	if err := binder.TrackSession(ctx, "sess_abc", 42, 7); err != nil {
		t.Fatalf("未装配时 TrackSession 应静默跳过: %v", err)
	}
	if err := binder.TrackObservedSession(ctx, "sess_abc"); err != nil {
		t.Fatalf("未装配时 TrackObservedSession 应静默跳过: %v", err)
	}
	if err := binder.RefreshObservedSession(ctx, "sess_abc", 0); err != nil {
		t.Fatalf("未装配时 RefreshObservedSession 应静默跳过: %v", err)
	}
	if removed, err := binder.TerminateObservedSession(ctx, "sess_abc"); err != nil || removed {
		t.Fatalf("未装配时 TerminateObservedSession 应静默返回: removed=%v err=%v", removed, err)
	}
	if err := binder.MarkLastSeen(ctx, "sess_abc", 0); err != nil {
		t.Fatalf("未装配时 MarkLastSeen 应静默跳过: %v", err)
	}
}

// ReadinessOfNilBinder 报告 nil 门面是否就绪（nil 必须视为未就绪，否则守卫链会去调用空指针）。
func ReadinessOfNilBinder() bool {
	var binder *Binder
	return binder.Ready()
}

// TestBinderInvalidInputIsConflictNotError 钉住输入预检：非法入参是 conflict(invalid_input)，
// 不是 error。两者在守卫链上是「回退 legacy」与「500」的区别，不能混淆。
func TestBinderInvalidInputIsConflictNotError(t *testing.T) {
	ctx := context.Background()
	binder := NewBinder(unreachableRedisClient(t))

	cases := []struct {
		name string
		call func() (BindingResult, error)
	}{
		{"keyID 为 0", func() (BindingResult, error) {
			return binder.ReadOrReconcile(ctx, "sess_abc", 0, 300)
		}},
		{"keyID 为负", func() (BindingResult, error) {
			return binder.ReadOrReconcile(ctx, "sess_abc", -1, 300)
		}},
		{"TTL 为 0", func() (BindingResult, error) {
			return binder.ReadOrReconcile(ctx, "sess_abc", 42, 0)
		}},
		{"TTL 为负", func() (BindingResult, error) {
			return binder.ReadOrReconcile(ctx, "sess_abc", 42, -5)
		}},
		{"CAS 缺代际", func() (BindingResult, error) {
			return binder.CompareAndSet(ctx, "sess_abc", 42, "", 1, 300)
		}},
		{"CAS 供应商为 0", func() (BindingResult, error) {
			return binder.CompareAndSet(ctx, "sess_abc", 42, "gen-1", 0, 300)
		}},
		{"Touch 缺代际", func() (BindingResult, error) {
			return binder.Touch(ctx, "sess_abc", 42, "", 1, 300)
		}},
		{"Clear 缺代际", func() (BindingResult, error) {
			return binder.Clear(ctx, "sess_abc", 42, "", 1, 0, 0, 300)
		}},
		{"Clear 冷却时长为负", func() (BindingResult, error) {
			return binder.Clear(ctx, "sess_abc", 42, "gen-1", 1, 0, -1, 300)
		}},
		{"Clear 冷却时长有值但期望供应商为空", func() (BindingResult, error) {
			return binder.Clear(ctx, "sess_abc", 42, "gen-1", 0, 0, 60, 300)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.call()
			if err != nil {
				t.Fatalf("非法入参不应返回 error: %v", err)
			}
			if result.OK || result.ConflictReason != "invalid_input" {
				t.Fatalf("非法入参应返回 conflict(invalid_input): %+v", result)
			}
		})
	}
}

// TestBinderInvalidInputLease 钉住租约的非法入参语义：抢租约按「被占用」处理，
// 续期/释放按「已丢失」处理——调用方据此决定是否重新抢与是否降级。
func TestBinderInvalidInputLease(t *testing.T) {
	ctx := context.Background()
	binder := NewBinder(unreachableRedisClient(t))

	for _, tc := range []struct {
		name      string
		sessionID string
		keyID     int64
		ttl       int
	}{
		{"会话 id 为空", "", 42, 300},
		{"keyID 为 0", "sess_abc", 0, 300},
		{"TTL 为 0", "sess_abc", 42, 0},
	} {
		t.Run("抢租约/"+tc.name, func(t *testing.T) {
			result, err := binder.AcquireLease(ctx, tc.sessionID, tc.keyID, "", tc.ttl)
			if err != nil {
				t.Fatalf("非法入参不应返回 error: %v", err)
			}
			if result.Acquired || !result.Conflict {
				t.Fatalf("非法入参应报冲突: %+v", result)
			}
		})
	}

	for _, tc := range []struct{ name, sessionID, token string }{
		{"token 为空", "sess_abc", ""},
		{"会话 id 为空", "", "token"},
	} {
		t.Run("续期/"+tc.name, func(t *testing.T) {
			result, err := binder.RenewLease(ctx, tc.sessionID, 42, tc.token, 300)
			if err != nil {
				t.Fatalf("非法入参不应返回 error: %v", err)
			}
			if result.OK || !result.Lost {
				t.Fatalf("非法入参应报丢失: %+v", result)
			}
		})
		t.Run("释放/"+tc.name, func(t *testing.T) {
			result, err := binder.ReleaseLease(ctx, tc.sessionID, 42, tc.token)
			if err != nil {
				t.Fatalf("非法入参不应返回 error: %v", err)
			}
			if result.OK || !result.Lost {
				t.Fatalf("非法入参应报丢失: %+v", result)
			}
		})
	}

	if result, err := binder.RenewLease(ctx, "sess_abc", 42, "token", 0); err != nil || !result.Lost {
		t.Fatalf("TTL 为 0 应报丢失: %+v err=%v", result, err)
	}
}

// TestParseStringsResult 覆盖 Lua 契约解析：四元组形态、允许来源、冲突原因折叠。
func TestParseStringsResult(t *testing.T) {
	allowed := bindingAllowedSources(SourceUpdated)

	cases := []struct {
		name        string
		values      []string
		allow       map[BindingSource]bool
		wantOK      bool
		wantSource  BindingSource
		wantGen     string
		wantProvide int64
		wantReason  string
		wantErr     bool
	}{
		{
			name: "更新成功", values: []string{"ok", "updated", "gen-2", "9"}, allow: allowed,
			wantOK: true, wantSource: SourceUpdated, wantGen: "gen-2", wantProvide: 9,
		},
		{
			name: "空供应商", values: []string{"ok", "updated", "gen-2", ""}, allow: allowed,
			wantOK: true, wantSource: SourceUpdated, wantGen: "gen-2",
		},
		{
			name:   "冲突原因已知",
			values: []string{"conflict", "generation_mismatch"}, allow: allowed, wantReason: "generation_mismatch",
		},
		{
			name:   "冲突原因未知则折叠",
			values: []string{"conflict", "brand_new_reason"}, allow: allowed, wantReason: "unknown_conflict",
		},
		{
			name:   "来源不在允许集合",
			values: []string{"ok", "created", "gen-2", "1"}, allow: allowed, wantErr: true,
		},
		{
			name:   "缺少 generation",
			values: []string{"ok", "updated", "", "1"}, allow: allowed, wantErr: true,
		},
		{
			name:   "元组不足四个",
			values: []string{"ok", "updated"}, allow: allowed, wantErr: true,
		},
		{
			name:   "结果整体不足两个",
			values: []string{"ok"}, allow: allowed, wantErr: true,
		},
		{
			name:   "未知首元素",
			values: []string{"weird", "updated", "gen-2", "1"}, allow: allowed, wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseStringsResult("sess_abc", 42, tc.values, tc.allow)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应返回 error: %+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应返回 error: %v", err)
			}
			if result.OK != tc.wantOK {
				t.Fatalf("OK 不符: %+v", result)
			}
			if !tc.wantOK {
				if result.ConflictReason != tc.wantReason {
					t.Fatalf("冲突原因不符: got=%q want=%q", result.ConflictReason, tc.wantReason)
				}
				return
			}
			if result.Source != tc.wantSource || result.Snapshot.Generation != tc.wantGen ||
				result.Snapshot.ProviderID != tc.wantProvide {
				t.Fatalf("快照不符: %+v", result)
			}
			if result.Snapshot.SessionID != "sess_abc" || result.Snapshot.KeyID != 42 {
				t.Fatalf("快照未回填会话/密钥: %+v", result.Snapshot)
			}
		})
	}
}

// TestKnownConflictReasonsCoversNodeSet 钉住冲突原因集合：漏一个会让真实原因被折叠成
// unknown_conflict，排障时看不出是「迟到写入被 fence 拒绝」还是别的问题。
func TestKnownConflictReasonsCoversNodeSet(t *testing.T) {
	// 与 src/lib/redis/session-binding.ts 的 CONFLICT_REASONS 逐字一致。
	nodeReasons := []string{
		"canonical_corrupt", "canonical_exists", "canonical_key_mismatch", "canonical_missing",
		"foreign_legacy_owner", "generation_mismatch", "invalid_input", "invalid_legacy_provider",
		"lease_held", "mirror_conflict", "mirror_missing", "not_owner_or_missing",
		"orphan_legacy_provider", "provider_mismatch", "unknown_conflict",
	}
	for _, reason := range nodeReasons {
		if !knownConflictReasons[reason] {
			t.Fatalf("缺少冲突原因 %q", reason)
		}
	}
	if len(knownConflictReasons) != len(nodeReasons) {
		t.Fatalf("冲突原因数量不符: got=%d want=%d", len(knownConflictReasons), len(nodeReasons))
	}
}

// TestEvalHelpers 覆盖 Lua 回复归一与正整数判定。
func TestEvalHelpers(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"字符串", "gen-1", "gen-1"},
		{"整数", int64(9), "9"},
		{"字节切片", []byte("bytes"), "bytes"},
		{"nil", nil, ""},
		{"无法归一", 1.5, ""},
	}
	for _, tc := range cases {
		t.Run("归一/"+tc.name, func(t *testing.T) {
			if got := evalString(tc.in); got != tc.want {
				t.Fatalf("归一不符: got=%q want=%q", got, tc.want)
			}
		})
	}

	for input, want := range map[string]bool{
		"1": true, "42": true, "0": true, "": false, "-1": false, "1a": false, " 1": false,
	} {
		if got := isPositiveIntegerString(input); got != want {
			t.Fatalf("正整数判定不符: %q got=%v want=%v", input, got, want)
		}
	}
}

// TestConflictReasonFolding 钉住「迟到 generation 被 fence 拒绝」在 Go 侧的可判别性：
// 上层据此决定是否重新读取绑定而不是重试同一次写入。
func TestConflictReasonFolding(t *testing.T) {
	result, err := parseStringsResult(
		"sess_abc", 42,
		[]string{"conflict", "generation_mismatch"},
		bindingAllowedSources(SourceUpdated),
	)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if result.OK {
		t.Fatal("fence 拒绝不应返回 OK")
	}
	if !strings.Contains(result.ConflictReason, "generation") {
		t.Fatalf("冲突原因应可判别为代际不符: %q", result.ConflictReason)
	}
}
