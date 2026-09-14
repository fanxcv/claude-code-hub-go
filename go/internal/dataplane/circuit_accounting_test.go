package dataplane

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// TestShouldRecordEndpointFailure 钉住端点级记账的窄判定（Node forwarder.ts:2569）：
// 只有超时与系统错误算端点问题，普通 5xx 只记供应商级。
func TestShouldRecordEndpointFailure(t *testing.T) {
	cases := []struct {
		name    string
		failure *forward.Failure
		want    bool
	}{
		{"nil 不记", nil, false},
		{"无端点不记", &forward.Failure{ProviderID: 1, StatusCode: 524}, false},
		{
			"524 超时记端点",
			&forward.Failure{ProviderID: 1, EndpointID: 7, StatusCode: 524, Category: forward.CategoryProviderError},
			true,
		},
		{
			"系统错误记端点",
			&forward.Failure{ProviderID: 1, EndpointID: 7, Category: forward.CategorySystemError},
			true,
		},
		{
			"普通 500 不记端点",
			&forward.Failure{ProviderID: 1, EndpointID: 7, StatusCode: 500, Category: forward.CategoryProviderError},
			false,
		},
		{
			"401 不记端点",
			&forward.Failure{ProviderID: 1, EndpointID: 7, StatusCode: 401, Category: forward.CategoryProviderError},
			false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := shouldRecordEndpointFailure(testCase.failure); got != testCase.want {
				t.Fatalf("shouldRecordEndpointFailure = %v，期望 %v", got, testCase.want)
			}
		})
	}
}
