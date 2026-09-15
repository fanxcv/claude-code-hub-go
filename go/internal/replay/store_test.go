package replay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 跨语言契约：Redis 键形制、热层 TTL、owner 租约 TTL 必须与 Node 的 replay-store.ts 逐字一致，
// 否则切换期间两侧无法互相命中同一条回放条目。这里的字面量即为契约，禁止顺手改名。
func TestKeyShapeMatchesNode(t *testing.T) {
	const replayID = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"owner", ownerKey(replayID), "cch:replay:owner:" + replayID},
		{"meta", metaKey(replayID), "cch:replay:meta:" + replayID},
		{"chunks", chunksKey(replayID), "cch:replay:chunks:" + replayID},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s 键形制不符: %q != %q", tc.name, tc.got, tc.want)
		}
	}
}

// TTL 默认值对齐 env.schema 的 REPLAY_TTL_SECONDS=600 与 replay-store.ts 的 OWNER_LEASE_TTL_SECONDS=45。
func TestTTLDefaultsMatchNode(t *testing.T) {
	if DefaultTTLSeconds != 600 {
		t.Fatalf("默认 TTL 应为 600 秒，实际 %d", DefaultTTLSeconds)
	}
	if ownerLeaseTTLSeconds != 45 {
		t.Fatalf("owner 租约 TTL 应为 45 秒，实际 %d", ownerLeaseTTLSeconds)
	}
	if got := (&Store{ttl: DefaultTTLSeconds * time.Second}).TTLSeconds(); got != 600 {
		t.Fatalf("TTLSeconds 应为 600，实际 %d", got)
	}
	if got := (&Store{ttl: 90 * time.Second}).TTLSeconds(); got != 90 {
		t.Fatalf("自定义 TTL 应生效，实际 %d", got)
	}
}

// 真实 Redis 上的 TTL 断言：owner 键带租约 TTL，meta/chunks 键带热层 TTL。
func TestRedisKeyTTLsAgainstNodeContract(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	replayID := identityReplayFor("ttl-shape-" + itNonce())
	t.Cleanup(func() {
		_ = client.Del(
			context.Background(), ownerKey(replayID), metaKey(replayID), chunksKey(replayID),
		).Err()
	})

	storeInstance, err := NewStore(StoreOptions{Redis: client})
	if err != nil {
		t.Fatalf("构造存储失败: %v", err)
	}
	if storeInstance.TTLSeconds() != 600 {
		t.Fatalf("默认热层 TTL 应为 600 秒，实际 %d", storeInstance.TTLSeconds())
	}
	if !storeInstance.TryClaimOwner(ctx, replayID, "owner-token-ttl") {
		t.Fatal("claim 失败")
	}
	meta := &Meta{Status: MetaOwning, Verifier: "v", ScopeTag: "s", Delivery: DeliveryStream}
	if _, outcome := storeInstance.WriteOwned(
		ctx, replayID, "owner-token-ttl", meta, []string{"data: x\n\n"},
	); outcome != WriteOK {
		t.Fatalf("写入失败: %v", outcome)
	}

	assertTTLNear := func(key string, wantSeconds int64) {
		t.Helper()
		want := time.Duration(wantSeconds) * time.Second
		ttl, err := client.TTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("读 %s TTL 失败: %v", key, err)
		}
		if ttl <= 0 || ttl > want {
			t.Fatalf("%s TTL 越界: %v（应约为 %ds）", key, ttl, wantSeconds)
		}
		if ttl < want-5*time.Second {
			t.Fatalf("%s TTL 过短: %v（应约为 %ds）", key, ttl, wantSeconds)
		}
	}
	assertTTLNear(ownerKey(replayID), ownerLeaseTTLSeconds)
	assertTTLNear(metaKey(replayID), 600)
	assertTTLNear(chunksKey(replayID), 600)

	// 心跳：仍持租约时刷新（含 meta 与现存 LIST 的 TTL），易主后返回 false。
	if !storeInstance.HeartbeatOwned(ctx, replayID, "owner-token-ttl", time.Now().UnixMilli()) {
		t.Fatal("仍持租约时心跳应成功")
	}
	assertTTLNear(ownerKey(replayID), ownerLeaseTTLSeconds)
	assertTTLNear(chunksKey(replayID), 600)
	if storeInstance.HeartbeatOwned(ctx, replayID, "someone-else", time.Now().UnixMilli()) {
		t.Fatal("非 owns 心跳必须失败")
	}
}

// 持久行一致性判定：任一内容维度不一致就不算同一个 winner（决定 existing 还是冲突）。
func TestSamePersistedRowRejectsMismatch(t *testing.T) {
	payload := "data: a\n\n"
	model := "gpt-test"
	base := PersistedRow{
		ReplayID: "r", Verifier: "v", ScopeTag: "s", KeyID: 1, UserID: 2, Format: "anthropic",
		Model: &model, StatusCode: 200, Headers: map[string]string{"content-type": "text/event-stream"},
		Payload: payload, ByteSize: int64(len(payload)),
	}
	if !samePersistedRow(&base, &base) {
		t.Fatal("同一行应判为一致")
	}
	cases := []struct {
		name   string
		mutate func(row *PersistedRow)
	}{
		{"verifier", func(row *PersistedRow) { row.Verifier = "other" }},
		{"scopeTag", func(row *PersistedRow) { row.ScopeTag = "other" }},
		{"keyId", func(row *PersistedRow) { row.KeyID = 9 }},
		{"userId", func(row *PersistedRow) { row.UserID = 9 }},
		{"format", func(row *PersistedRow) { row.Format = "openai" }},
		{"model 变空", func(row *PersistedRow) { row.Model = nil }},
		{"model 变值", func(row *PersistedRow) { other := "gpt-other"; row.Model = &other }},
		{"statusCode", func(row *PersistedRow) { row.StatusCode = 500 }},
		{"headers 增键", func(row *PersistedRow) { row.Headers["x-extra"] = "1" }},
		{"headers 改值", func(row *PersistedRow) { row.Headers["content-type"] = "application/json" }},
		{"payload", func(row *PersistedRow) { row.Payload = "data: b\n\n" }},
		{"byteSize", func(row *PersistedRow) { row.ByteSize = 1 }},
	}
	for _, tc := range cases {
		candidate := base
		headerCopy := map[string]string{}
		for key, value := range base.Headers {
			headerCopy[key] = value
		}
		candidate.Headers = headerCopy
		tc.mutate(&candidate)
		if samePersistedRow(&candidate, &base) {
			t.Fatalf("%s 不一致时不得判为同一行", tc.name)
		}
	}
}

// durable 持久化必须全局串行（Node 的 serializeDurablePersistence 单链）：并发收尾时
// 同一时刻只允许一处正文重建在堆上，否则 N 条流收尾会叠加成 N 倍正文峰值。
func TestDurablePersistenceSerialized(t *testing.T) {
	const workers = 8
	var (
		inFlight    int
		peak        int
		observed    sync.Mutex
		start       = make(chan struct{})
		completions sync.WaitGroup
	)
	completions.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer completions.Done()
			<-start
			_, _ = withDurablePersistence(func() (struct{}, error) {
				observed.Lock()
				inFlight++
				if inFlight > peak {
					peak = inFlight
				}
				observed.Unlock()
				time.Sleep(2 * time.Millisecond)
				observed.Lock()
				inFlight--
				observed.Unlock()
				return struct{}{}, nil
			})
		}()
	}
	close(start)
	completions.Wait()
	if peak != 1 {
		t.Fatalf("durable 持久化未串行，并发峰值 %d", peak)
	}
}

// fakeSystemSettings 是 SystemSettingsSource 的桩：只提供回放 TTL 一项，并记录读取次数。
type fakeSystemSettings struct {
	minutes int
	err     error
	calls   int
}

func (f *fakeSystemSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &store.SystemSettings{ReplayCacheTTLMinutes: f.minutes}, nil
}

// TestTTLSecondsFollowsReplayCacheSetting 钉住 Node 的 resolveReplayTtlSeconds：
// min(REPLAY_TTL_SECONDS, clamp(replay_cache_ttl_minutes, 5, 120, 默认 30) * 60)。
//
// 跨语言契约：改设置必须同时改两边的热层 TTL，否则切换期间同一条回放条目会在两侧
// 不同时刻过期（Node 写、Go 读）→ 读端凭空 miss。
func TestTTLSecondsFollowsReplayCacheSetting(t *testing.T) {
	cases := []struct {
		name      string
		envTTL    time.Duration
		minutes   int
		settings  bool
		want      int64
		readFails bool
	}{
		{name: "设置默认 30 分钟时不越过 env 上界", envTTL: 600 * time.Second, minutes: 30, settings: true, want: 600},
		{name: "设置 5 分钟时取小", envTTL: 600 * time.Second, minutes: 5, settings: true, want: 300},
		{name: "设置 120 分钟仍不越过 env 上界", envTTL: 600 * time.Second, minutes: 120, settings: true, want: 600},
		{name: "env 上界更小时取 env", envTTL: 900 * time.Second, minutes: 120, settings: true, want: 900},
		{name: "0 分钟按 Node 夹到下限 5", envTTL: 600 * time.Second, minutes: 0, settings: true, want: 300},
		{name: "超上限按 Node 夹到 120", envTTL: 7200 * time.Second, minutes: 500, settings: true, want: 7200},
		{name: "未接线设置时用默认 30 分钟", envTTL: 600 * time.Second, settings: false, want: 600},
		{name: "读设置失败时回落默认", envTTL: 600 * time.Second, settings: true, readFails: true, want: 600},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var source SystemSettingsSource
			if testCase.settings {
				fake := &fakeSystemSettings{minutes: testCase.minutes}
				if testCase.readFails {
					fake.err = errors.New("库不可用")
				}
				source = fake
			}
			store := &Store{ttl: testCase.envTTL, settings: source, now: time.Now}
			if got := store.ttlSeconds(context.Background()); got != testCase.want {
				t.Fatalf("ttlSeconds 期望 %d 秒，实际 %d 秒", testCase.want, got)
			}
		})
	}
}

// TestTTLSecondsCachesSettingsSnapshot 钉住设置快照的缓存窗口（Node CACHE_TTL_MS = 60 秒）：
// 窗口内改设置沿用旧值（避免逐 chunk 读库），过期后生效（不必重启进程）。
func TestTTLSecondsCachesSettingsSnapshot(t *testing.T) {
	fake := &fakeSystemSettings{minutes: 5}
	clock := time.Now()
	store := &Store{ttl: 3600 * time.Second, settings: fake, now: func() time.Time { return clock }}

	if got := store.ttlSeconds(context.Background()); got != 300 {
		t.Fatalf("首次应取设置值 5 分钟 = 300 秒，实际 %d", got)
	}
	fake.minutes = 60
	if got := store.ttlSeconds(context.Background()); got != 300 {
		t.Fatalf("缓存窗口内不得重新读库（应仍为 300 秒），实际 %d", got)
	}
	if fake.calls != 1 {
		t.Fatalf("缓存窗口内只应读库一次，实际 %d 次", fake.calls)
	}

	clock = clock.Add(settingsCacheTTL + time.Second)
	if got := store.ttlSeconds(context.Background()); got != 3600 {
		t.Fatalf("缓存过期后应取新设置（60 分钟与 env 上界取小 = 3600 秒），实际 %d", got)
	}
	if fake.calls != 2 {
		t.Fatalf("缓存过期后应再读一次库，实际共 %d 次", fake.calls)
	}
}
