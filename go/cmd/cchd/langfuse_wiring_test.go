package main

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住 LANGFUSE_* 的**开关语义与装配接线**。
//
// 背景：这组变量长期只出现在配置契约与启动摘要里，Go 侧没有任何上报实现——即「运维填了 key
// 也不会有数据，且全套用例照绿」。故这里既断言开关行为，也用源码钉子守住那段需真库才能跑到的
// 大装配（与 dataplane 侧的 tracer_wiring_nail_test.go 同一套做法）。

func langfuseEnv(publicKey, secretKey *string) config.EnvConfig {
	return config.EnvConfig{
		LangfuseBaseURL:    "https://langfuse.example.com",
		LangfusePublicKey:  publicKey,
		LangfuseSecretKey:  secretKey,
		LangfuseSampleRate: 1,
	}
}

func strPtr(value string) *string { return &value }

// 缺任一 key 即整体关闭：返回 nil，调用方（装配行）拿到的是真 nil 接口。
func TestNewLangfuseTracerDisabledWithoutKeys(t *testing.T) {
	cases := map[string]config.EnvConfig{
		"两个都缺":      langfuseEnv(nil, nil),
		"只给 public": langfuseEnv(strPtr("pk"), nil),
		"只给 secret": langfuseEnv(nil, strPtr("sk")),
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if tracer := newLangfuseTracer(env, logx.New(io.Discard)); tracer != nil {
				t.Fatalf("缺 key 时必须整体关闭（返回 nil）")
			}
		})
	}
}

// 两个 key 齐备即启用，且收尾可关闭（不得 panic）。
func TestNewLangfuseTracerEnabledWithKeys(t *testing.T) {
	tracer := newLangfuseTracer(langfuseEnv(strPtr("pk"), strPtr("sk")), logx.New(io.Discard))
	if tracer == nil {
		t.Fatalf("两个 key 齐备时应启用上报")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tracer.Close(closeCtx)
}

// 生产装配必须真的把上报器接进数据面，并在收尾时关闭：这两行都在需要真库的函数里，
// 摘掉任一行都不会让别的用例变红（那正是「静默不生效」的形态）。
func TestLangfuseProductionWiringNail(t *testing.T) {
	raw, err := os.ReadFile("dataplane.go")
	if err != nil {
		t.Fatalf("读 dataplane.go 失败: %v", err)
	}
	source := string(raw)
	for name, pattern := range map[string]string{
		"装配注入": `Tracer: tracing.AsTracer(traces),`,
		"开关构造": `traces := newLangfuseTracer(options.Cfg.Env, options.Logger)`,
		"收尾关闭": `traces.Close(closeCtx)`,
	} {
		if !strings.Contains(source, pattern) {
			t.Fatalf("数据面装配里找不到「%s」：少了它，配了 LANGFUSE_* 也不会有上报（或收尾不 flush），"+
				"而其余用例不会变红", name)
		}
	}
	if !regexp.MustCompile(`(?m)^const langfuseCloseTimeout`).MatchString(source) {
		t.Fatalf("收尾等待应有上界（langfuseCloseTimeout）")
	}
}
