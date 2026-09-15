package dataplane

import (
	"io"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// stubBodyAccess 是最小的守卫正文缝隙替身：同一棵树被读写，与真实访问器语义一致。
type stubBodyAccess struct {
	body  map[string]any
	store int
}

func (s *stubBodyAccess) JSON() (map[string]any, error) { return s.body, nil }

func (s *stubBodyAccess) Store(body map[string]any) error {
	s.body = body
	s.store++
	return nil
}

// newResponsesInputFactory 造一个包了 responses input 归一的正文工厂，并返回被包装的内层访问器。
func newResponsesInputFactory(
	t *testing.T,
	state *RequestState,
	switches rectify.Switches,
	inner *stubBodyAccess,
) guard.BodyFactory {
	t.Helper()
	factory := guard.BodyFactory(func(*pctx.Context) (guard.BodyAccess, error) { return inner, nil })
	wrapped := wrapResponsesInputBodyFactory(factory, state, func() rectify.Switches { return switches }, logx.New(io.Discard))
	if wrapped == nil {
		t.Fatal("包装后的工厂为 nil")
	}
	return wrapped
}

// TestResponsesInputBodyNormalizesBeforeGuard 断言 responses 路由的 `input` 在**首次取正文时**就归一，
// 使守卫链的过滤器、建行审计与转换器看到同一份形状（Node 在 pipeline 之前归一）。
func TestResponsesInputBodyNormalizesBeforeGuard(t *testing.T) {
	inner := &stubBodyAccess{body: map[string]any{"model": "gpt-5.2", "input": "hello"}}
	state := &RequestState{}
	factory := newResponsesInputFactory(t, state, rectify.NodeDefaults(), inner)

	access, err := factory(nil)
	if err != nil {
		t.Fatalf("取正文访问器失败: %v", err)
	}
	body, err := access.JSON()
	if err != nil {
		t.Fatalf("取正文失败: %v", err)
	}

	items, ok := body["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input 应被归一为数组: %#v", body["input"])
	}
	first, _ := items[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("归一后的消息形状不对: %#v", first)
	}
	if inner.store != 1 {
		t.Fatalf("归一必须写回正文（守卫链后续步骤与转发都读它），实际写回 %d 次", inner.store)
	}
	entries := state.rectifierAuditsSnapshot()
	if len(entries) != 1 {
		t.Fatalf("应产生一条审计条目，实际 %d 条", len(entries))
	}
	entry := entries[0]
	if entry["type"] != specialsettings.TypeResponseInputRectifier || entry["hit"] != true {
		t.Fatalf("审计条目不对: %#v", entry)
	}
	if entry["action"] != "string_to_array" || entry["originalType"] != "string" {
		t.Fatalf("审计字段不对: %#v", entry)
	}
	if _, hasProvider := entry["providerId"]; hasProvider {
		t.Fatalf("主动型条目不应带 provider 上下文: %#v", entry)
	}
}

// TestResponsesInputBodySkipsWhenSwitchOff 断言开关关闭时正文原样、无审计。
func TestResponsesInputBodySkipsWhenSwitchOff(t *testing.T) {
	inner := &stubBodyAccess{body: map[string]any{"input": "hello"}}
	state := &RequestState{}
	switches := rectify.NodeDefaults()
	switches.ResponseInput = false
	factory := newResponsesInputFactory(t, state, switches, inner)

	access, _ := factory(nil)
	body, err := access.JSON()
	if err != nil {
		t.Fatalf("取正文失败: %v", err)
	}
	if body["input"] != "hello" || inner.store != 0 || len(state.rectifierAuditsSnapshot()) != 0 {
		t.Fatalf("开关关闭时不应改动正文或写审计: %#v store=%d audits=%d", body, inner.store, len(state.rectifierAuditsSnapshot()))
	}
}

// TestResponsesInputBodyIsIdempotent 断言数组形态不二次整流（重放与多次读正文都不会重复写审计）。
func TestResponsesInputBodyIsIdempotent(t *testing.T) {
	inner := &stubBodyAccess{body: map[string]any{"input": []any{map[string]any{"role": "user"}}}}
	state := &RequestState{}
	factory := newResponsesInputFactory(t, state, rectify.NodeDefaults(), inner)

	for i := 0; i < 3; i++ {
		access, _ := factory(nil)
		if _, err := access.JSON(); err != nil {
			t.Fatalf("第 %d 次取正文失败: %v", i+1, err)
		}
	}
	if inner.store != 0 {
		t.Fatalf("数组形态不应触发写回，实际 %d 次", inner.store)
	}
	if len(state.rectifierAuditsSnapshot()) != 0 {
		t.Fatalf("数组形态不应产生审计，实际 %d 条", len(state.rectifierAuditsSnapshot()))
	}
}
