package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 亲和开关的合成规则：**总闸与模式各自独立**（2026-09-21 拆分，见 drizzle/0132）。
//
// 总闸沿袭 Node 的合成口径（config.ts:11-13 的 `env || settings`）；模式只取自系统设置，
// 与总闸无关。拆分前两者挤在同一字段 `affinityIgnoreClientSessionId`，翻它的默认值会把
// 整套亲和关掉，保持 true 又无法让会话粘性生效——本测试把拆分后的不变式钉住。
func TestAffinityDecisionMirrorsNodeSemantics(t *testing.T) {
	enabled, disabled := true, false
	cases := []struct {
		name         string
		envEnabled   bool
		settings     *store.SystemSettings
		settingsErr  error
		wantEnabled  bool
		wantSource   string
		wantIgnore   bool // 模式开关的生效值（与总闸无关）
		wantSettings bool // 是否记录了系统设置读取失败
	}{
		{
			name:        "env 开、总闸关：env 强制",
			envEnabled:  true,
			settings:    &store.SystemSettings{AffinityEnabled: disabled},
			wantEnabled: true,
			wantSource:  "env",
		},
		{
			name:        "env 关、总闸开：系统设置驱动（产品默认）",
			settings:    &store.SystemSettings{AffinityEnabled: enabled},
			wantEnabled: true,
			wantSource:  "system_setting",
		},
		{
			name:        "两路都开：来源标明两路",
			envEnabled:  true,
			settings:    &store.SystemSettings{AffinityEnabled: enabled},
			wantEnabled: true,
			wantSource:  "env+system_setting",
		},
		{
			name:       "总闸两路都关",
			settings:   &store.SystemSettings{AffinityEnabled: disabled},
			wantSource: "disabled",
		},
		{
			name:         "系统设置读取失败：按出厂默认（总闸开、模式为会话优先）处理并记录",
			settingsErr:  context.DeadlineExceeded,
			wantEnabled:  true,
			wantSource:   "system_setting",
			wantSettings: true,
		},
		{
			// 拆分的核心不变式：模式为真**不得**把总闸关掉。
			// 拆分前 AffinityIgnoreClientSessionID 一字段两用，此用例在旧语义下无法表达。
			name:        "模式=强制前缀、总闸开：会话粘性关闭但亲和整体仍活",
			settings:    &store.SystemSettings{AffinityEnabled: enabled, AffinityIgnoreClientSessionID: enabled},
			wantEnabled: true,
			wantSource:  "system_setting",
			wantIgnore:  true,
		},
		{
			// 模式独立于总闸：总闸关时模式字段照旧如实回报（它是配置事实，不是结论）。
			name:       "模式=强制前缀、总闸关：模式仍如实回报，总闸仍为关",
			settings:   &store.SystemSettings{AffinityEnabled: disabled, AffinityIgnoreClientSessionID: enabled},
			wantSource: "disabled",
			wantIgnore: true,
		},
		{
			name:        "模式=会话优先（出厂方向）：忽略会话为假",
			settings:    &store.SystemSettings{AffinityEnabled: enabled},
			wantEnabled: true,
			wantSource:  "system_setting",
			wantIgnore:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := affinityDecision(tc.envEnabled, tc.settings, tc.settingsErr)
			if status.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v，期望 %v", status.Enabled, tc.wantEnabled)
			}
			if status.Source != tc.wantSource {
				t.Errorf("Source = %q，期望 %q", status.Source, tc.wantSource)
			}
			if status.IgnoreClientSessionID != tc.wantIgnore {
				t.Errorf("IgnoreClientSessionID = %v，期望 %v", status.IgnoreClientSessionID, tc.wantIgnore)
			}
			if (status.SettingsErr != "") != tc.wantSettings {
				t.Errorf("SettingsErr = %q，期望有值=%v", status.SettingsErr, tc.wantSettings)
			}
		})
	}
}

// 未配置 Redis：亲和存储必须为 nil（选择器据此不查找也不提名），且不得 panic。
func TestOpenAffinityWithoutRedisBuildsNoStore(t *testing.T) {
	var logs strings.Builder
	setup := openAffinity(context.Background(), config.Config{}, nil, logx.New(&logs))
	if setup.store != nil {
		t.Fatal("没有 Redis 时不得建亲和存储")
	}
	if setup.status.Enabled {
		t.Fatalf("没有 Redis 时不得报告启用: %+v", setup.status)
	}
	if !strings.Contains(logs.String(), "affinity_absent") {
		t.Fatalf("必须留下 affinity_absent 日志: %s", logs.String())
	}
}

// 集成：真实 PG + Redis —— 存储被建出来、window/TTL 来自配置、TTL 真的生效、
// close 之后连接已释放。这条覆盖「装配了但没生效」与「关了但连接泄漏」两类缺陷。
func TestIntegrationOpenAffinityBuildsStoreFromConfig(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	redisURL := os.Getenv("CCH_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("未设置 CCH_TEST_DSN / CCH_TEST_REDIS_URL，跳过集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(4),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-affinity-boot",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	cfg := config.Config{RedisURL: redisURL}
	cfg.Env.PrefixAffinityWindow = 4
	cfg.Env.PrefixAffinityTTLSeconds = 240

	setup := openAffinity(ctx, cfg, pools, logx.New(io.Discard))
	if setup.store == nil {
		t.Skip("系统设置与 env 均未启用亲和，本环境不适用")
	}
	if !setup.status.Enabled {
		t.Fatalf("应报告启用: %+v", setup.status)
	}
	if setup.status.Window != 4 || setup.status.TTLSeconds != 240 {
		t.Fatalf("window/TTL 必须来自配置: %+v", setup.status)
	}
	if setup.status.Source == "" || setup.status.Source == "disabled" {
		t.Fatalf("启用时必须给出开关来源: %+v", setup.status)
	}

	// TTL 生效证明：预置 identity generation，再按选路写回一个绑定，读回 TTL 应为配置值。
	client := terminalTestRedis(t, redisURL)
	scope := "boot" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		keys, _, scanErr := client.Scan(ctx, 0, "cch:pfx:{"+scope+":*", 200).Result()
		if scanErr == nil && len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
	})
	tip := "tip-" + scope
	if err := client.Set(ctx, "cch:pfx:{"+scope+"}:gen:"+tip, "v3:boot", time.Hour).Err(); err != nil {
		t.Fatalf("预置 generation 失败: %v", err)
	}
	if !setup.store.Put(ctx, scope, tip, 7, tip, "v3:boot") {
		t.Fatal("写回应成功")
	}
	ttl, err := client.TTL(ctx, "cch:pfx:{"+scope+"}:fp:"+tip).Result()
	if err != nil {
		t.Fatalf("读取 TTL 失败: %v", err)
	}
	if ttl < 200*time.Second || ttl > 240*time.Second {
		t.Fatalf("绑定 TTL = %s，期望落在 (200s, 240s]（配置值 240s）", ttl)
	}

	setup.close()
	if err := setup.client.Ping(ctx).Err(); err == nil {
		t.Fatal("close 之后亲和连接必须已释放")
	}
}

// /readyz 的亲和结论必须自带「启动快照」标注，且 env 派生的 window/ttl 仍照旧带出。
//
// 为什么必须钉：Enabled / Source / IgnoreClientSessionID 取自启动时读到的系统设置（装配时经
// AffinityReport 一次性回填），而对应的选路已改成**逐请求读**、运行时改设置**立即**生效。
// 不标注就会让运维把 /readyz 读成活值，进而得出「改了没生效」的错误结论——
// 与「接口返 200 而行为不变」同一族误导。本用例同时钉住「不得把 env 那半也标成快照」。
func TestAffinityDescribeMarksBootSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		status affinityStatus
	}{
		{
			name:   "启用：来源与模式均带标注",
			status: affinityStatus{Enabled: true, Source: "system_setting", Window: 8, TTLSeconds: 3600},
		},
		{
			// 关掉时同样必须标注：运维正靠着看为什么亲和没开。
			name:   "关闭：仍带标注",
			status: affinityStatus{Source: "disabled"},
		},
		{
			name: "设置读取失败：错误说明与标注共存",
			status: affinityStatus{
				Enabled: true, Source: "system_setting", Window: 8, TTLSeconds: 3600,
				SettingsErr: "dial tcp: 连接超时",
			},
		},
		{
			name: "模式=强制前缀：标注不受模式影响",
			status: affinityStatus{
				Enabled: true, Source: "env+system_setting", Window: 8, TTLSeconds: 3600,
				IgnoreClientSessionID: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.status.describe()
			if !strings.Contains(got, affinityBootSnapshotNote) {
				t.Fatalf("结论必须自带启动快照标注，收到 %q", got)
			}
			// 标注必须说清两件事：这半是启动快照且运行时改设置不刷新它。
			for _, want := range []string{"启动快照", "本行不更新"} {
				if !strings.Contains(got, want) {
					t.Errorf("标注应含 %q，收到 %q", want, got)
				}
			}
			// env 派生的那半不得被当成快照一起标：它本就是启动常量，但必须仍被如实带出。
			if tc.status.Enabled && !strings.Contains(got, fmt.Sprintf("window %d", tc.status.Window)) {
				t.Errorf("env 派生的 window 必须照旧带出，收到 %q", got)
			}
			if !strings.Contains(got, "取自 env") {
				t.Errorf("window/ttl 的 env 出处必须写明，收到 %q", got)
			}
		})
	}
}

// /readyz 的配置结论里，affinity 一行必须带上「启动快照」标注；翻设置**不得**让它看起来像活值。
//
// 为何在 /readyz 这一层再钉一次：describe() 的单测只证「串里含标注」，本用例证「该串真的
// 经 readyProber 走到了 notes 里」。而「改设置后本行不变」是**设计事实**（装配时一次性
// 回填），所以钉的是「标注在场」而不是「值变新」——后者才需要改行为，超出本次授权。
func TestReadyzAffinityNoteCarriesBootSnapshotMarker(t *testing.T) {
	prober := readyProber{affinity: affinityStatus{
		Enabled: true, Source: "system_setting", Window: 8, TTLSeconds: 3600,
	}}
	note, ok := prober.DataPlaneConfiguration()["affinity"]
	if !ok || note == "" {
		t.Fatal("/readyz 必须报出 affinity 一行")
	}
	if !strings.Contains(note, affinityBootSnapshotNote) {
		t.Fatalf("affinity 一行必须自带启动快照标注，收到 %q", note)
	}
	if !strings.Contains(note, "window 8") || !strings.Contains(note, "ttl 3600s") {
		t.Fatalf("env 派生的 window/ttl 必须照旧带出，收到 %q", note)
	}
}

// terminalTestRedis 建一条测试用 Redis 客户端（库号缺省 13，避免碰生产键空间）。
func terminalTestRedis(t *testing.T, raw string) redis.UniversalClient {
	t.Helper()
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}
