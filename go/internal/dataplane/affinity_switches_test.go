package dataplane

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「亲和开关的逐请求读取面真的从 system_settings 快照取值，而不是构造期快照」。
//
// 为什么必须单独钉：设计稿 §7 把管理面 affinityIgnoreClientSessionId 明定为**配置回滚手段**。
// 若这一跳取的是构造期值，管理面返回 200、失效广播也发了，运行中的选路却不变——回滚失败且
// 全程无错无日志。本仓「已定义≠已接线」已犯多次，形态正是「函数写了、没人接」。

// stubSettingsSource 是最小设置读取面：每次调用都从闭包取值，便于模拟「运行时被改了」。
type stubSettingsSource struct {
	current func() (*store.SystemSettings, error)
}

func (s stubSettingsSource) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return s.current()
}

// TestAffinitySwitchesForReadsSnapshotPerRequest 主线：同一 gate 连续调用，快照一变结论就变。
func TestAffinitySwitchesForReadsSnapshotPerRequest(t *testing.T) {
	snapshot := &store.SystemSettings{AffinityEnabled: true}
	gate := affinitySwitchesFor(StoreOptions{}, stubSettingsSource{
		current: func() (*store.SystemSettings, error) { return snapshot, nil },
	})

	if got := gate(context.Background()); !got.Enabled || got.ForcePrefix {
		t.Fatalf("初始 = %+v，期望总闸开、模式为会话优先", got)
	}

	// 管理面把模式改成强制前缀：下一次读必须立刻反映（这就是那条回滚手段）。
	snapshot = &store.SystemSettings{AffinityEnabled: true, AffinityIgnoreClientSessionID: true}
	if got := gate(context.Background()); !got.ForcePrefix {
		t.Fatalf("模式改成强制前缀后 = %+v，期望 ForcePrefix=true", got)
	}

	// 管理面关掉总闸（env 未开）：整层不进。
	snapshot = &store.SystemSettings{}
	if got := gate(context.Background()); got.Enabled {
		t.Fatalf("总闸关后 = %+v，期望 Enabled=false", got)
	}

	// env 强制开（ENABLE_PREFIX_AFFINITY）：快照关也不关总闸（env || settings，与 affinityDecision 同序）。
	envGate := affinitySwitchesFor(StoreOptions{AffinityEnvEnabled: true}, stubSettingsSource{
		current: func() (*store.SystemSettings, error) { return &store.SystemSettings{}, nil },
	})
	if got := envGate(context.Background()); !got.Enabled {
		t.Fatalf("env 强制开时 = %+v，期望 Enabled=true", got)
	}
}

// TestAffinitySwitchesForFailOpen 读设置失败/无读取面时按出厂默认（总闸开、模式会话优先），
// 与 cmd/cchd 的 affinityDecision 同一口径：两侧不一致才是缺陷。
func TestAffinitySwitchesForFailOpen(t *testing.T) {
	broken := affinitySwitchesFor(StoreOptions{}, stubSettingsSource{
		current: func() (*store.SystemSettings, error) { return nil, errors.New("boom") },
	})
	if got := broken(context.Background()); !got.Enabled || got.ForcePrefix {
		t.Fatalf("读取失败 = %+v，期望按出厂默认（总闸开、会话优先）", got)
	}
	if got := affinitySwitchesFor(StoreOptions{}, nil)(context.Background()); !got.Enabled || got.ForcePrefix {
		t.Fatalf("无读取面 = %+v，期望按出厂默认（总闸开、会话优先）", got)
	}
}

// TestAffinitySwitchesForHonorsInjectedGate 调用方显式注入的读取面优先（测试用），不被覆盖。
func TestAffinitySwitchesForHonorsInjectedGate(t *testing.T) {
	injected := func(context.Context) route.AffinitySwitches {
		return route.AffinitySwitches{Enabled: true, ForcePrefix: true}
	}
	gate := affinitySwitchesFor(StoreOptions{
		RouteOptions: route.Options{AffinitySwitches: injected},
	}, stubSettingsSource{current: func() (*store.SystemSettings, error) { return &store.SystemSettings{}, nil }})
	if got := gate(context.Background()); !got.Enabled || !got.ForcePrefix {
		t.Fatalf("注入的读取面应原样生效，实际 %+v", got)
	}
}

// TestAssemblyWiresAffinitySwitches 源码结构钉子：装配处**真的**把逐请求读取面交给了选器。
//
// 为什么用源码结构断言：整条装配链需要真库、真 Redis 才跑得起来，而这条接线的失效形态正是
// 「函数写了、没人接」——那时单测全绿而生产永远用静态值。与同目录的 slow_probe_wiring_nail_test.go
// 同一手法。
func TestAssemblyWiresAffinitySwitches(t *testing.T) {
	source, err := os.ReadFile("assemble.go")
	if err != nil {
		t.Fatalf("读取 assemble.go 失败: %v", err)
	}
	text := string(source)
	if !strings.Contains(text, "AffinitySwitches:") || !strings.Contains(text, "affinitySwitchesFor(") {
		t.Fatal("assemble.go 里应把 affinitySwitchesFor(...) 接到 route.Options.AffinitySwitches 上；" +
			"缺了它，亲和开关只在启动时取值（管理面热更新变成假象）")
	}
}
