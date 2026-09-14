package terminal

import "testing"

// 终态判据的用例与库内函数 fn_is_message_request_finalized 的分支一一对应；
// 集成测试会用同一组用例对库内函数比对（TestIsFinalizedMatchesDatabaseFunction）。
func TestIsFinalizedMirrorsDatabaseFunction(t *testing.T) {
	emptyText := ""
	blocked := "sensitive_word"
	statusCode := 200
	errorMessage := "upstream aborted"

	cases := []struct {
		name  string
		facts FinalizationFacts
		want  bool
	}{
		{
			name:  "全空即未终态",
			facts: FinalizationFacts{},
			want:  false,
		},
		{
			name:  "blocked_by 非空即终态",
			facts: FinalizationFacts{BlockedBy: &blocked},
			want:  true,
		},
		{
			name:  "status_code 非空即终态",
			facts: FinalizationFacts{StatusCode: &statusCode},
			want:  true,
		},
		{
			name:  "error_message 非空即终态",
			facts: FinalizationFacts{ErrorMessage: &errorMessage},
			want:  true,
		},
		{
			name:  "error_message 为空串不算终态",
			facts: FinalizationFacts{ErrorMessage: &emptyText},
			want:  false,
		},
		{
			name:  "链路末段 reason 在白名单内即终态",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"hedge_loser_billed"}]`)},
			want:  true,
		},
		{
			name:  "链路末段 reason 不在白名单则看状态码",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"initial_selection"}]`)},
			want:  false,
		},
		{
			name:  "链路末段带数值型 statusCode 即终态",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"initial_selection","statusCode":503}]`)},
			want:  true,
		},
		{
			name:  "链路末段 statusCode 为字符串不算数值",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"initial_selection","statusCode":"503"}]`)},
			want:  false,
		},
		{
			name:  "链路末段带非空 errorMessage 即终态",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"initial_selection","errorMessage":"boom"}]`)},
			want:  true,
		},
		{
			name:  "只看末段：前段的终态信号不算",
			facts: FinalizationFacts{ProviderChain: []byte(`[{"reason":"retry_success"},{"reason":"initial_selection"}]`)},
			want:  false,
		},
		{
			name:  "空数组合法但不算终态",
			facts: FinalizationFacts{ProviderChain: []byte(`[]`)},
			want:  false,
		},
		{
			name:  "末段不是对象不算终态",
			facts: FinalizationFacts{ProviderChain: []byte(`[1,2,3]`)},
			want:  false,
		},
		{
			name:  "非法 json 不算终态（不 panic）",
			facts: FinalizationFacts{ProviderChain: []byte(`{`)},
			want:  false,
		},
		{
			name:  "空字节不算终态",
			facts: FinalizationFacts{ProviderChain: []byte("  ")},
			want:  false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsFinalized(testCase.facts); got != testCase.want {
				t.Fatalf("IsFinalized = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestClassifyMapsFactsToState(t *testing.T) {
	statusCode := 502
	if got := Classify(FinalizationFacts{}); got != StateOpen {
		t.Fatalf("空事实应判为 open，得到 %s", got)
	}
	if got := Classify(FinalizationFacts{StatusCode: &statusCode}); got != StateSettled {
		t.Fatalf("有状态码应判为 settled，得到 %s", got)
	}
}
