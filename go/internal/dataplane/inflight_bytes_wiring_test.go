package dataplane

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住 CCH_GO_MAX_INFLIGHT_BYTES 的唯一生效路径：
// StoreOptions.MaxInflightBytes → newBodyOptions → 正文准入层。
//
// 改前必红：装配处根本不引用 options.MaxInflightBytes，Admission 恒为 nil，于是实际预算
// 来自 guard 包单例自己再读一次同名环境变量——config 里那份值（及其区间校验与启动日志）
// 是一份死的第二真源，日志打印的上限与实际生效的上限可以不一致。
func TestMaxInflightBytesDrivesAdmissionBudget(t *testing.T) {
	const budget = 8 << 20
	options := newBodyOptions(budget)
	if options.Admission == nil {
		t.Fatal("装配必须显式注入 Admission：nil 会让读体落到 guard 包单例，配置值即失效")
	}
	if got := options.Admission.Stats().Body.MaxBytes; got != budget {
		t.Fatalf("在途正文上限 = %d，期望取自配置的 %d", got, budget)
	}
}

// TestMaxInflightBytesRejectsOverBudgetBody 端到端证明确实是这份配置在拒。
//
// 走 guard.NewBodyAccessor（生产读体路径）而不是只看 Stats：Stats 一致但读体时用别的
// 准入实例，正是这条链接线前的真实缺陷形态。
func TestMaxInflightBytesRejectsOverBudgetBody(t *testing.T) {
	const budget = 8
	options := newBodyOptions(budget)

	payload := []byte(`{"model":"claude-sonnet-4-5"}`)
	header := http.Header{}
	header.Set("Content-Length", strconv.Itoa(len(payload)))
	ctx, err := pctx.New(pctx.Init{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: header,
		Body:    io.NopCloser(bytes.NewReader(payload)),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}

	_, err = guard.NewBodyAccessor(ctx, options)
	if !errors.Is(err, ingress.ErrBodyBudgetExhausted) {
		t.Fatalf("超出配置上限应被拒为 ErrBodyBudgetExhausted，得到 %v", err)
	}
	if !ingress.IsCapacityError(err) {
		t.Fatalf("准入拒绝必须可被 IsCapacityError 识别（映射 503），得到 %v", err)
	}
}

// TestBodyOptionsSharedByBothConsumers 钉住「进程级只有一份」：守卫链的每请求工厂与
// 模型列表路由都读 h.options.BodyOptions，装配处必须给同一个实例，否则在途总量被记两本账。
func TestBodyOptionsSharedByBothConsumers(t *testing.T) {
	options := newBodyOptions(1024)
	again := newBodyOptions(1024)
	if options.Admission == again.Admission {
		t.Fatal("两处新调 newBodyOptions 不应共用实例——共享必须由装配处传同一个值达成")
	}
	if options.Ingress.Decompression == nil {
		t.Fatal("解压限流器必须非 nil：nil 表示不做在途准入，与装配前的行为不一致")
	}
}
