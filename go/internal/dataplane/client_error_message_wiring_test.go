package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// passthroughSettingsStub 是 pass_through_upstream_error_message 的设置桩。
type passthroughSettingsStub struct{ enabled bool }

func (f passthroughSettingsStub) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{PassThroughUpstreamErrorMessage: f.enabled}, nil
}

// TestFailoverStatusForPassThroughSwitch 钉住上游错误文案的开关两态。
//
// 判据来自 Node error-handler.ts:121-158：关闭时用状态码对应的通用文案；打开时用从上游
// 原文派生的客户端安全文案，派生不出来才回退通用文案。
func TestFailoverStatusForPassThroughSwitch(t *testing.T) {
	handler := func(enabled bool) *Handler {
		return &Handler{
			options: Options{Base: guard.Deps{Settings: passthroughSettingsStub{enabled: enabled}}},
			logger:  logx.New(nil),
		}
	}
	upstreamFailure := &forward.Failure{
		StatusCode:   400,
		Message:      "context length exceeded",
		ProviderID:   3,
		ProviderName: "供应商甲",
	}

	t.Run("开关关闭：通用文案", func(t *testing.T) {
		status, message := handler(false).failoverStatusFor(context.Background(), upstreamFailure)
		if status != 400 {
			t.Errorf("状态码 = %d，期望 400（上游状态原样透传）", status)
		}
		if message != "上游请求参数无效，请检查后重试" {
			t.Errorf("文案 = %q，期望通用文案", message)
		}
	})

	t.Run("开关打开：派生上游文案", func(t *testing.T) {
		_, message := handler(true).failoverStatusFor(context.Background(), upstreamFailure)
		if message != "context length exceeded" {
			t.Errorf("文案 = %q，期望派生后的上游文案", message)
		}
	})

	t.Run("开关打开但文案含内部信息：回退通用文案", func(t *testing.T) {
		failure := &forward.Failure{
			StatusCode:   400,
			Message:      "no available providers",
			ProviderID:   3,
			ProviderName: "供应商甲",
		}
		_, message := handler(true).failoverStatusFor(context.Background(), failure)
		if message != "上游请求参数无效，请检查后重试" {
			t.Errorf("文案 = %q，期望回退通用文案", message)
		}
	})

	t.Run("本进程造的失败不受开关影响", func(t *testing.T) {
		// 传输层超时由本进程归因，文案不含上游内部信息；替换成通用文案反而丢诊断信息。
		failure := &forward.Failure{
			StatusCode: 504,
			Message:    "上游超时",
			Internal:   true,
		}
		if _, message := handler(false).failoverStatusFor(context.Background(), failure); message != "上游超时" {
			t.Errorf("文案 = %q，期望保持本进程归因", message)
		}
	})

	t.Run("空归因不 panic", func(t *testing.T) {
		status, message := handler(true).failoverStatusFor(context.Background(), nil)
		if status != 502 || message == "" {
			t.Errorf("空归因下 status=%d message=%q，期望 502 与非空文案", status, message)
		}
	})
}

// TestVerboseProviderErrorSwitch 钉住 verbose_provider_error 的读设置两态。
func TestVerboseProviderErrorSwitch(t *testing.T) {
	handler := func(verbose bool) *Handler {
		return &Handler{
			options: Options{Base: guard.Deps{Settings: verboseSettingsStub{verbose: verbose}}},
			logger:  logx.New(nil),
		}
	}
	if !handler(true).verboseProviderError(context.Background()) {
		t.Error("设置打开时 verboseProviderError 应为 true")
	}
	if handler(false).verboseProviderError(context.Background()) {
		t.Error("设置关闭时 verboseProviderError 应为 false")
	}
	// 未接线设置源时按简洁模式（Node 默认，且不向客户端暴露内部事实）。
	bare := &Handler{options: Options{}, logger: logx.New(nil)}
	if bare.verboseProviderError(context.Background()) {
		t.Error("未接线时应按简洁模式")
	}
}

// verboseSettingsStub 是 verbose_provider_error 的设置桩。
type verboseSettingsStub struct{ verbose bool }

func (f verboseSettingsStub) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{VerboseProviderError: f.verbose}, nil
}
