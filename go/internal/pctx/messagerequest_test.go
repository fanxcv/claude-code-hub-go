package pctx

import (
	"errors"
	"testing"
)

// TestMessageRequestIDIsWriteOnce 钉住「一条请求只开一行」：标识只能写一次，重复写入必须报错。
func TestMessageRequestIDIsWriteOnce(t *testing.T) {
	ctx, err := New(Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}

	if id, ok := ctx.MessageRequestID(); ok || id != 0 {
		t.Fatalf("未开行时应为「无标识」，得到 id=%d ok=%v", id, ok)
	}

	if err := ctx.SetMessageRequestID(42); err != nil {
		t.Fatalf("首次写入应成功: %v", err)
	}
	id, ok := ctx.MessageRequestID()
	if !ok || id != 42 {
		t.Fatalf("读取标识失败: id=%d ok=%v", id, ok)
	}

	// 第二次写入（含同值）必须被拒，且不得改动既有值。
	if err := ctx.SetMessageRequestID(43); !errors.Is(err, ErrMessageRequestIDAlreadySet) {
		t.Fatalf("重复写入应返回 ErrMessageRequestIDAlreadySet，得到 %v", err)
	}
	if err := ctx.SetMessageRequestID(42); !errors.Is(err, ErrMessageRequestIDAlreadySet) {
		t.Fatalf("同值重复写入同样应被拒，得到 %v", err)
	}
	if id, _ := ctx.MessageRequestID(); id != 42 {
		t.Fatalf("被拒的写入改动了既有标识: %d", id)
	}
}

// TestMessageRequestIDRejectsNonPositive 钉住非正数非法：行 id 由数据库序列给出，0/负数是构造错误。
func TestMessageRequestIDRejectsNonPositive(t *testing.T) {
	ctx, err := New(Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	for _, id := range []int64{0, -1} {
		if err := ctx.SetMessageRequestID(id); !errors.Is(err, ErrInvalidMessageRequestID) {
			t.Fatalf("id=%d 应返回 ErrInvalidMessageRequestID，得到 %v", id, err)
		}
	}
	if _, ok := ctx.MessageRequestID(); ok {
		t.Fatalf("非法写入不应占住槽位")
	}
}
