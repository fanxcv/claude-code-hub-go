package guard

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住正文准入占位的生命周期：占位随访问器存活（而不是构造器返回即归还），到 Release
// 才归还，构造失败则立即归还。
//
// 为什么必须钉：占位是「在途正文」的唯一度量。构造器一归还会让闸门只数到读体那一瞬，而访问器
// 仍持有原文与解析树直到请求终态——在途计数因此与真实驻留脱钩，任何按百分比定的阈值都失真。

// leaseTestBody 是准入用例的正文（与 body_release_test.go 的语料同形，但两处各自独立读数）。
const leaseTestBody = `{"model":"claude-sonnet-4-5"}`

// leaseTestAdmission 给一份「只够一份正文」的预算：Multiplier 固定 1，把记账系数与生命周期
// 解耦（系数另有用例），任何未归还的占位都会在第二次申请时表面化。
func leaseTestAdmission(bodyBytes int) *ingress.Admission {
	return ingress.NewAdmission(ingress.AdmissionOptions{
		MaxInflightBodyBytes: int64(bodyBytes),
		Multiplier:           1,
	})
}

// leaseTestContext 造一个声明了 content-length 的请求上下文。
//
// 必须声明长度：准入申请取的是「解压后字节的下界」，头里没有长度时申请 0，闸门看不到占用，
// 用例就变成空转。带编码时解压后字节不可预知，申请退回用压缩体长度，同样是正数。
func leaseTestContext(t *testing.T, payload []byte, contentEncoding string) *pctx.Context {
	t.Helper()
	header := http.Header{}
	header.Set("Content-Length", strconv.Itoa(len(payload)))
	if contentEncoding != "" {
		header.Set("Content-Encoding", contentEncoding)
	}
	ctx, err := pctx.New(pctx.Init{
		Method: "POST", Path: "/v1/messages", Headers: header,
		Body: io.NopCloser(bytes.NewReader(payload)),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// TestBodyAccessorLeaseHeldWhileAlive 钉住占位随访问器存活：构造器返回后仍在在途记账里。
//
// 改前必红：占位在构造器里 defer 归还，这里读到的是 0。
func TestBodyAccessorLeaseHeldWhileAlive(t *testing.T) {
	admission := leaseTestAdmission(len(leaseTestBody))
	accessor, err := NewBodyAccessor(
		leaseTestContext(t, []byte(leaseTestBody), ""), BodyAccessOptions{Admission: admission},
	)
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}
	defer accessor.Release()

	stats := admission.Stats().Body
	if want := int64(len(leaseTestBody)); stats.ActiveBytes != want {
		t.Fatalf("访问器存活期间在途记账 = %d，期望 = 正文 %d：构造器返回即归还是本用例要修的失真",
			stats.ActiveBytes, want)
	}
	if stats.Active != 1 {
		t.Fatalf("访问器存活期间在途名额 = %d，期望 1", stats.Active)
	}
}

// TestBodyAccessorReleaseReturnsLease 钉住 Release 归还占位，且重复调用不 panic、不重复扣减。
func TestBodyAccessorReleaseReturnsLease(t *testing.T) {
	admission := leaseTestAdmission(len(leaseTestBody))
	accessor, err := NewBodyAccessor(
		leaseTestContext(t, []byte(leaseTestBody), ""), BodyAccessOptions{Admission: admission},
	)
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}

	accessor.Release()
	if got := admission.Stats().Body.ActiveBytes; got != 0 {
		t.Fatalf("Release 后在途记账 = %d，期望 0", got)
	}
	// 重复释放：不得 panic，也不得把名额/字节扣成负数（负值等于把预算凭空放大）。
	accessor.Release()
	if stats := admission.Stats().Body; stats.Active != 0 || stats.ActiveBytes != 0 {
		t.Fatalf("重复 Release 后读数 = %+v，期望名额与字节均为 0", stats)
	}

	// 端口钉子：额度是真的可用，而不是只在读数上归零。
	again, err := NewBodyAccessor(
		leaseTestContext(t, []byte(leaseTestBody), ""), BodyAccessOptions{Admission: admission},
	)
	if err != nil {
		t.Fatalf("释放后额度应可再用: %v", err)
	}
	again.Release()
}

// TestReleaseTreeKeepsLease 钉住「释放树不还额度」。
//
// ReleaseTree 之后原文仍可读（转发、回放与定点重写都还要用它），真实驻留没有减少，故额度
// 不能跟着树一起还——否则长流式请求会在最占内存的那一段把额度让给下一条请求。
func TestReleaseTreeKeepsLease(t *testing.T) {
	admission := leaseTestAdmission(len(leaseTestBody))
	accessor, err := NewBodyAccessor(
		leaseTestContext(t, []byte(leaseTestBody), ""), BodyAccessOptions{Admission: admission},
	)
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}
	defer accessor.Release()

	accessor.ReleaseTree()
	if want := int64(len(leaseTestBody)); admission.Stats().Body.ActiveBytes != want {
		t.Fatalf("ReleaseTree 后在途记账 = %d，期望仍占正文 %d",
			admission.Stats().Body.ActiveBytes, want)
	}
	if kept, err := accessor.Bytes(); err != nil || len(kept) == 0 {
		t.Fatalf("ReleaseTree 后原文应仍可读（这正是额度不能跟着还的原因）: %v", err)
	}

	accessor.Release()
	if got := admission.Stats().Body.ActiveBytes; got != 0 {
		t.Fatalf("Release 后在途记账 = %d，期望 0", got)
	}
}

// TestBodyAccessorLeaseReturnedOnConstructorFailure 钉住构造失败路径立即归还：失败请求占着额度
// 会把一次客户端错误放大成后续请求的 503。
func TestBodyAccessorLeaseReturnedOnConstructorFailure(t *testing.T) {
	cases := map[string]struct {
		payload  []byte
		encoding string
	}{
		"正文不是 JSON 对象": {payload: []byte(`[1,2,3]`)},
		"解压流损坏":        {payload: []byte("not a gzip stream"), encoding: "gzip"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			admission := leaseTestAdmission(len(testCase.payload))
			_, err := NewBodyAccessor(
				leaseTestContext(t, testCase.payload, testCase.encoding),
				BodyAccessOptions{Admission: admission},
			)
			if err == nil {
				t.Fatal("构造失败路径应返回错误")
			}
			if stats := admission.Stats().Body; stats.Active != 0 || stats.ActiveBytes != 0 {
				t.Fatalf("构造失败后在途读数 = %+v，期望已立即归还", stats)
			}
		})
	}
}
