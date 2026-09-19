package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
)

// TestDialOptionsFromEnvUsesFetchTimeouts 钉住装配缝确实把 FETCH_* 三档超时接进了拨号参数。
//
// 这三项此前只被解析、从未下传，运维改它们完全无声：拨号层照旧用自己的硬默认。
func TestDialOptionsFromEnvUsesFetchTimeouts(t *testing.T) {
	cases := []struct {
		name        string
		env         config.EnvConfig
		wantConnect time.Duration
		wantHeaders time.Duration
		wantBody    time.Duration
	}{
		{
			name: "契约默认值",
			env: config.EnvConfig{
				FetchConnectTimeout: 30000,
				FetchHeadersTimeout: 600000,
				FetchBodyTimeout:    600000,
			},
			wantConnect: dial.DefaultConnectTimeout,
			wantHeaders: dial.DefaultHeadersTimeout,
			wantBody:    dial.DefaultBodyIdleTimeout,
		},
		{
			name: "运维改过的值",
			env: config.EnvConfig{
				FetchConnectTimeout: 1500,
				FetchHeadersTimeout: 2500,
				FetchBodyTimeout:    3500,
			},
			wantConnect: 1500 * time.Millisecond,
			wantHeaders: 2500 * time.Millisecond,
			wantBody:    3500 * time.Millisecond,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := dialOptionsFromEnv(testCase.env)
			if options.ConnectTimeout != testCase.wantConnect {
				t.Fatalf("ConnectTimeout 期望 %v，实际 %v", testCase.wantConnect, options.ConnectTimeout)
			}
			if options.HeadersTimeout != testCase.wantHeaders {
				t.Fatalf("HeadersTimeout 期望 %v，实际 %v", testCase.wantHeaders, options.HeadersTimeout)
			}
			if options.BodyIdleTimeout != testCase.wantBody {
				t.Fatalf("BodyIdleTimeout 期望 %v，实际 %v", testCase.wantBody, options.BodyIdleTimeout)
			}
			if options.ConnectTimeout <= 0 || options.HeadersTimeout <= 0 || options.BodyIdleTimeout <= 0 {
				t.Fatalf("三个超时都必须来自配置（非零），实际 %+v", options)
			}
		})
	}
}

// TestDialOptionsFromEnvZeroMeansDialDefault 钉住「0 表示用默认」：未配置或非正数时回零，
// 由 dial 的 resolve 取 Default*，而不是在装配缝里写死一个数值。
func TestDialOptionsFromEnvZeroMeansDialDefault(t *testing.T) {
	options := dialOptionsFromEnv(config.EnvConfig{
		FetchConnectTimeout: 0,
		FetchHeadersTimeout: -1,
		FetchBodyTimeout:    0,
	})
	if options.ConnectTimeout != 0 || options.HeadersTimeout != 0 || options.BodyIdleTimeout != 0 {
		t.Fatalf("未配置/非法值必须回零交给 dial 兜底，实际 %+v", options)
	}
	client, err := dial.New(options)
	if err != nil {
		t.Fatalf("零值 Options 必须可用: %v", err)
	}
	client.Close()
}

// TestDialOptionsFromEnvValueReachesDialClient 用真实超时行为证明配置值一路走到拨号层：
// 配置 120ms、上游迟迟不给响应头时必须以 ErrHeadersTimeout 判类——硬默认 600s 下不会触发。
func TestDialOptionsFromEnvValueReachesDialClient(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	client, err := dial.New(dialOptionsFromEnv(config.EnvConfig{
		FetchConnectTimeout: 30000,
		FetchHeadersTimeout: 120,
		FetchBodyTimeout:    600000,
	}))
	if err != nil {
		t.Fatalf("dial.New 失败: %v", err)
	}
	defer client.Close()

	_, err = client.RoundTrip(context.Background(), dial.Request{
		Method:        http.MethodPost,
		URL:           server.URL,
		ContentLength: 0,
	})
	if !errors.Is(err, dial.ErrHeadersTimeout) {
		t.Fatalf("等响应头超时必须以 ErrHeadersTimeout 分类，实际 %v", err)
	}
}
