package guard

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住「会话身份的来源从守卫侧折到选路侧」这一跳。
//
// 为什么必须单独钉：前缀兜底是否参与，取决于 route.Request.SessionIdentity；而它由会话
// 守卫步骤按 SessionResult.IdentitySource 折出来。这一跳丢了（或折错方向），前缀兜底要么
// 永不触发、要么对客户端显式带 id 的请求也生效——两种都是静默的行为偏移。
func TestRouteSessionIdentityMapsAllSources(t *testing.T) {
	cases := []struct {
		name string
		in   SessionIdentitySource
		want route.SessionIdentity
	}{
		{name: "客户端显式携带", in: SessionIdentityClient, want: route.SessionIdentityClient},
		{name: "按哈希找回", in: SessionIdentityRecovered, want: route.SessionIdentityRecovered},
		{name: "网关生成", in: SessionIdentityGenerated, want: route.SessionIdentityGenerated},
		// 未知取值一律按「客户端显式携带」：那是接线前的行为，也是最保守的一侧
		// （不退化成「所有请求都走前缀兜底」）。
		{name: "未知取值", in: SessionIdentitySource(99), want: route.SessionIdentityClient},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := routeSessionIdentity(testCase.in); got != testCase.want {
				t.Errorf("routeSessionIdentity(%v) = %v，期望 %v", testCase.in, got, testCase.want)
			}
		})
	}
}
