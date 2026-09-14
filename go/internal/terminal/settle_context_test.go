package terminal

import (
	"context"
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// newTestContext 造一个最小可用的请求上下文。
func newTestContext(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// TestSettleContextUpdatesRowOpenedByGuard 钉住生产路径：守卫链开了行，终态结算更新那一行，
// 不再建第二行，也不再自己生成 id。
func TestSettleContextUpdatesRowOpenedByGuard(t *testing.T) {
	ctx := newTestContext(t)
	if err := ctx.SetMessageRequestID(77); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, Options{})

	result, err := settler.SettleContext(context.Background(), ctx, Settlement{StatusCode: 200}, nil)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if !result.Committed || result.Attempts != 1 {
		t.Fatalf("结算结果不符: %+v", result)
	}
	if writer.createCall != 0 {
		t.Fatalf("上下文已有行标识时不得再建行：createCall=%d", writer.createCall)
	}
	if len(writer.unfinalizedIDs) != 1 || writer.unfinalizedIDs[0] != 77 {
		t.Fatalf("终态写未落在 guard 开的行上: %v", writer.unfinalizedIDs)
	}
}

// TestSettleContextCreatesRowWhenNoID 钉住向后兼容：上下文没有行标识（守卫链未开行）时，
// 给出开行载荷即按「建行 + 终态」两步结算。
func TestSettleContextCreatesRowWhenNoID(t *testing.T) {
	ctx := newTestContext(t)
	writer := &fakeWriter{
		createRow:        store.MessageRequest{ID: 91},
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
	}
	settler := New(writer, Options{})

	payload := store.CreateMessageRequestData{UserID: 5, Key: "sk-test"}
	blockedBy := "sensitive_word"
	result, err := settler.SettleContext(context.Background(), ctx, Settlement{
		StatusCode: 400,
		BlockedBy:  &blockedBy,
	}, &payload)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatalf("结算未提交: %+v", result)
	}
	if writer.createCall != 1 {
		t.Fatalf("应建一行：createCall=%d", writer.createCall)
	}
	if len(writer.unfinalizedIDs) != 1 || writer.unfinalizedIDs[0] != 91 {
		t.Fatalf("终态写未落在新建行上: %v", writer.unfinalizedIDs)
	}
}

// TestSettleContextReportsNoRow 钉住第三分支：既无标识也无开行载荷时不落库，且给出可判别结论。
//
// 这条分支对应「守卫链没有开行」的请求（无鉴权/无供应商，与 Node 侧 messageContext 为空一致）：
// 必须报出来，不能被当成一次成功的结算。
func TestSettleContextReportsNoRow(t *testing.T) {
	writer := &fakeWriter{}
	settler := New(writer, Options{})

	for name, pc := range map[string]*pctx.Context{"无标识": newTestContext(t), "上下文为空": nil} {
		result, err := settler.SettleContext(context.Background(), pc, Settlement{StatusCode: 200}, nil)
		if !errors.Is(err, ErrNoRow) {
			t.Fatalf("%s：应返回 ErrNoRow，得到 %v", name, err)
		}
		if result.Committed {
			t.Fatalf("%s：不应报告已提交: %+v", name, result)
		}
	}
	if writer.createCall != 0 || writer.unfinalizedCalls != 0 {
		t.Fatalf("无行可结算时不得触库: create=%d unfinalized=%d", writer.createCall, writer.unfinalizedCalls)
	}
}

// TestSettleContextIsIdempotentPerRow 钉住「同一请求只结算一次」：第二次结算被 store 的终态谓词挡住。
//
// 进程内不加闸（见 SettleContext 的说明），所以这里断言的是数据库侧屏障的可见后果：
// 第二次调用得到 ErrNotSettled，而不是再写一次终态。
func TestSettleContextIsIdempotentPerRow(t *testing.T) {
	ctx := newTestContext(t)
	if err := ctx.SetMessageRequestID(11); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}, {committed: false}}}
	settler := New(writer, Options{})

	if _, err := settler.SettleContext(context.Background(), ctx, Settlement{StatusCode: 200}, nil); err != nil {
		t.Fatalf("首次结算失败: %v", err)
	}
	result, err := settler.SettleContext(context.Background(), ctx, Settlement{StatusCode: 500}, nil)
	if !errors.Is(err, ErrNotSettled) {
		t.Fatalf("二次结算应返回 ErrNotSettled，得到 %v", err)
	}
	if result.Committed {
		t.Fatalf("二次结算不应提交: %+v", result)
	}
	if len(writer.unfinalizedIDs) != 2 || writer.unfinalizedIDs[0] != 11 || writer.unfinalizedIDs[1] != 11 {
		t.Fatalf("两次都应针对同一行: %v", writer.unfinalizedIDs)
	}
}
