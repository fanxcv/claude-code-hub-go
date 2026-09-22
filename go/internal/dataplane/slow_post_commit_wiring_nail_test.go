package dataplane

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件钉住「提交后掉速」（二级闸 `OnPostCommitSlow`）真的接到了写面上。
//
// 为什么必须有它：这条链接的两段各自都有测试（采样器确有 OnDegraded 回调、写面确有
// RecordSlowPrecommit），但**「dataplane 到底有没有把回调填进 forward.StreamOptions」谁都盖不住**——
// 摘掉那一行，两侧用例全绿，而功能静默失效：二级闸判定降速后回调为 nil，什么都不做，
// 后续请求照旧选中那家。这正是「已定义≠已接线」的盲区（同类见同目录 slow_probe_wiring_nail_test.go）。

var postCommitSlowWiring = regexp.MustCompile(
	`(?m)^\s*streamOptions\.OnPostCommitSlow = h\.postCommitSlow\(state, requestCtx\)\s*$`)

var postCommitSlowSinkWiring = regexp.MustCompile(
	`(?m)^\s*PostCommitSlow: slowPostCommitSink\(slowRate\),\s*$`)

func TestStreamOptionsReceivePostCommitSlowCallback(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("dataplane.go"))
	if err != nil {
		t.Fatalf("读取 dataplane.go 失败：%v", err)
	}
	if !postCommitSlowWiring.Match(source) {
		t.Fatalf("dataplane.go 里找不到二级闸回调的接线：\n" +
			"  期望形如 `streamOptions.OnPostCommitSlow = h.postCommitSlow(state, requestCtx)`\n" +
			"  该行缺失时，二级闸判定降速后什么都不做（回调恒为 nil），后续请求照旧选中该家。")
	}

	assemble, err := os.ReadFile(filepath.Join("assemble.go"))
	if err != nil {
		t.Fatalf("读取 assemble.go 失败：%v", err)
	}
	if !postCommitSlowSinkWiring.Match(assemble) {
		t.Fatalf("assemble.go 里找不到写入面的接线：\n" +
			"  期望形如 `PostCommitSlow: slowPostCommitSink(slowRate),`\n" +
			"  该行缺失时回调即使被构造也会因 sink 为 nil 而整段跳过。")
	}
}

// TestPostCommitSlowWritesFactOnSameScope 钉住行为：回调触发时真的写出一条事实，
// 且**作用域与其它低速事实逐字同源**（模型键取自本次请求、成员是请求行 id）。
//
// 作用域同源是这条链的关键：模型键或请求 id 一旦与读侧/其它写侧不一致，写进的滑窗就不是
// 读侧要数的那个，标记静默隐形。
func TestPostCommitSlowWritesFactOnSameScope(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造 pctx 失败：%v", err)
	}
	const requestID = int64(4242)
	if err := pc.SetMessageRequestID(requestID); err != nil {
		t.Fatalf("写入请求行 id 失败：%v", err)
	}

	state := &RequestState{PC: pc, Model: "deepseek-v4.1-flash"}
	state.sessionID = "sess-1"

	got := make(chan terminal.SlowPrecommit, 1)
	handler := &Handler{
		options: Options{PostCommitSlow: func(_ context.Context, fact terminal.SlowPrecommit) {
			got <- fact
		}},
		logger: logx.New(io.Discard),
	}

	callback := handler.postCommitSlow(state, context.Background())
	if callback == nil {
		t.Fatal("已接线时应给出回调，得 nil")
	}
	callback(167, 42)

	select {
	case fact := <-got:
		if fact.ProviderID != 167 {
			t.Fatalf("渠道 = %d，期望 167", fact.ProviderID)
		}
		if fact.RequestID != requestID {
			t.Fatalf("幂等成员（请求行 id）= %d，期望 %d", fact.RequestID, requestID)
		}
		want := slowScopeFor(state, pc)
		if fact.ModelKey != want.ModelKey {
			t.Fatalf("模型键 = %q，期望 %q（必须与其它低速事实同源）", fact.ModelKey, want.ModelKey)
		}
		if fact.ModelKey == "" {
			t.Fatal("模型键不得为空：为空时写进去的键不是读侧要数的那个")
		}
		if fact.SessionID != "sess-1" {
			t.Fatalf("会话 id = %q，期望 sess-1", fact.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("回调未写出低速事实")
	}
}

// TestPostCommitSlowNilWithoutSink 钉住未接线时的行为：返回 nil（等效未装，forward 整段跳过），
// 而不是一个空转的回调。
func TestPostCommitSlowNilWithoutSink(t *testing.T) {
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造 pctx 失败：%v", err)
	}
	handler := &Handler{options: Options{}, logger: logx.New(io.Discard)}
	if callback := handler.postCommitSlow(&RequestState{PC: pc, Model: "m"}, context.Background()); callback != nil {
		t.Fatal("未接线时必须返回 nil（否则 forward 会认为已处置）")
	}
}
