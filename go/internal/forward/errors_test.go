package forward

import (
	"context"
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// stubRules 是固定答案的错误规则匹配器（不报 category：退化为最保守的一档）。
type stubRules struct{ match bool }

func (s stubRules) Matches(string) bool { return s.match }

// stubCategoryRules 是带 category 的错误规则匹配器（实现 forward.RuleCategoryMatcher）。
type stubCategoryRules struct {
	match      bool
	categories []string
}

func (s stubCategoryRules) Matches(string) bool { return s.match }

func (s stubCategoryRules) MatchedCategories(string) []string { return s.categories }

// TestCategoryForRuleCategory 钉住「规则 category -> 转发分类」的映射：只有一档不是客户输入错误。
func TestCategoryForRuleCategory(t *testing.T) {
	cases := []struct {
		name     string
		category string
		want     Category
	}{
		{"本档规则", RuleCategoryProviderUnsupportedInput, CategoryProviderUnsupportedInput},
		{"大小写与空白不敏感", "  Provider_Unsupported_Input  ", CategoryProviderUnsupportedInput},
		{"其它已知类型（Prompt 超限）", "prompt_limit", CategoryNonRetryableClientError},
		{"其它已知类型（非法请求）", "invalid_request", CategoryNonRetryableClientError},
		{"空字符串", "", CategoryNonRetryableClientError},
		{"未知值", "something_new", CategoryNonRetryableClientError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CategoryForRuleCategory(tc.category); got != tc.want {
				t.Fatalf("CategoryForRuleCategory(%q) = %v，期望 %v", tc.category, got, tc.want)
			}
		})
	}
}

// TestClassifyPriorityChain 断言分类优先级链的每一步次序。
func TestClassifyPriorityChain(t *testing.T) {
	cases := []struct {
		name string
		in   ClassifyInput
		want Category
	}{
		{
			name: "真实上游 5xx 先于客户端中断：正文写着 canceled 也算供应商故障",
			in:   ClassifyInput{StatusCode: 502, Err: dial.ErrContextCanceled, Body: `{"error":"request canceled"}`},
			want: CategoryProviderError,
		},
		{
			name: "合成 5xx（fake-200）不算真实传输状态，按客户端中断判定",
			in:   ClassifyInput{StatusCode: 502, Synthetic: true, Err: context.Canceled},
			want: CategoryClientAbort,
		},
		{
			name: "客户端中断先于本地过载",
			in:   ClassifyInput{Err: dial.ErrContextCanceled},
			want: CategoryClientAbort,
		},
		{
			name: "数据库连接池准入过载归本地过载",
			in:   ClassifyInput{Err: &store.AdmissionError{}},
			want: CategoryLocalOverload,
		},
		{
			name: "入站内存准入耗尽归本地过载",
			in:   ClassifyInput{Err: ingress.ErrInsufficientMemory},
			want: CategoryLocalOverload,
		},
		{
			name: "传输错误先于错误规则：正文命中规则也仍是系统错误",
			in:   ClassifyInput{Err: dial.ErrConnect, Body: "prompt is too long", Rules: stubRules{match: true}},
			want: CategorySystemError,
		},
		{
			name: "供应商局部模型缺口 404 先于规则匹配",
			in:   ClassifyInput{StatusCode: 404, ProviderLocalModelUnavailable: true, Body: "model_not_found", Rules: stubRules{match: true}},
			want: CategoryResourceNotFound,
		},
		{
			name: "以 400 回传的存储容量故障先于规则匹配",
			in:   ClassifyInput{StatusCode: 400, Body: "disk storage creation failed ... disk free-space floor reached", Rules: stubRules{match: true}},
			want: CategoryProviderError,
		},
		{
			name: "存储容量标记不完整则不算：必须全部命中",
			in:   ClassifyInput{StatusCode: 400, Body: "disk storage creation failed"},
			want: CategoryProviderError,
		},
		{
			name: "规则命中归客户端输入错误",
			in:   ClassifyInput{StatusCode: 400, Body: "prompt is too long", Rules: stubRules{match: true}},
			want: CategoryNonRetryableClientError,
		},
		{
			name: "规则命中且该类不是「供应商不支持该输入形态」：仍是不可重试的客户端错误",
			in: ClassifyInput{StatusCode: 400, Body: "prompt is too long",
				Rules: stubCategoryRules{match: true, categories: []string{"invalid_request"}}},
			want: CategoryNonRetryableClientError,
		},
		{
			name: "规则命中且该类是「供应商不支持该输入形态」：同家不重试、可换家",
			in: ClassifyInput{StatusCode: 400, Body: "image URLs are not currently supported",
				Rules: stubCategoryRules{match: true, categories: []string{RuleCategoryProviderUnsupportedInput}}},
			want: CategoryProviderUnsupportedInput,
		},
		{
			name: "两族同时命中取更保守的一档：不可重试客户端错误优先",
			in: ClassifyInput{StatusCode: 400, Body: "prompt is too long and image URLs are not supported",
				Rules: stubCategoryRules{match: true,
					categories: []string{RuleCategoryProviderUnsupportedInput, "invalid_request"}}},
			want: CategoryNonRetryableClientError,
		},
		{
			name: "匹配器报命中但拿不出 category：按最保守的一档算",
			in: ClassifyInput{StatusCode: 400, Body: "whatever",
				Rules: stubCategoryRules{match: true}},
			want: CategoryNonRetryableClientError,
		},
		{
			name: "匹配器不实现扩展接口：按最保守的一档算",
			in:   ClassifyInput{StatusCode: 400, Body: "whatever", Rules: stubRules{match: true}},
			want: CategoryNonRetryableClientError,
		},
		{
			name: "规则未命中时 400 仍是供应商故障",
			in:   ClassifyInput{StatusCode: 400, Body: "invalid request", Rules: stubRules{match: false}},
			want: CategoryProviderError,
		},
		{
			name: "无规则时 400 仍是供应商故障",
			in:   ClassifyInput{StatusCode: 400, Body: "prompt is too long"},
			want: CategoryProviderError,
		},
		{
			name: "404 归资源不存在",
			in:   ClassifyInput{StatusCode: 404},
			want: CategoryResourceNotFound,
		},
		{
			name: "空响应归供应商故障",
			in:   ClassifyInput{EmptyResponse: true},
			want: CategoryProviderError,
		},
		{
			name: "兜底为系统错误",
			in:   ClassifyInput{Err: errors.New("connection reset by peer")},
			want: CategorySystemError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Fatalf("Classify = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestCategoryRetryPredicates 断言分类到「重试 / 切换 / 计熔断」的映射。
func TestCategoryRetryPredicates(t *testing.T) {
	cases := []struct {
		category     Category
		retries      bool
		switches     bool
		circuit      bool
		categoryName string
	}{
		{CategoryProviderError, true, true, true, "provider_error"},
		{CategorySystemError, true, true, false, "system_error"},
		{CategoryResourceNotFound, true, true, false, "resource_not_found"},
		{CategoryClientAbort, false, false, false, "client_abort"},
		{CategoryNonRetryableClientError, false, false, false, "client_error_non_retryable"},
		{CategoryLocalOverload, false, false, false, "local_overload"},
		// 新增的一档：同家不重试（重试是白费）、可换家（另一家可能支持该形态）、不计熔断。
		{CategoryProviderUnsupportedInput, false, true, false, ReasonUnsupported},
	}

	for _, tc := range cases {
		t.Run(tc.categoryName, func(t *testing.T) {
			if got := tc.category.RetriesSameProvider(); got != tc.retries {
				t.Fatalf("RetriesSameProvider = %v，期望 %v", got, tc.retries)
			}
			if got := tc.category.SwitchesProvider(); got != tc.switches {
				t.Fatalf("SwitchesProvider = %v，期望 %v", got, tc.switches)
			}
			if got := tc.category.CountsTowardCircuit(); got != tc.circuit {
				t.Fatalf("CountsTowardCircuit = %v，期望 %v", got, tc.circuit)
			}
			if tc.category.String() != tc.categoryName {
				t.Fatalf("String = %q，期望 %q", tc.category.String(), tc.categoryName)
			}
		})
	}
}

// TestIsLocalOverloadError 断言本地过载判定只认本进程的两类过载。
func TestIsLocalOverloadError(t *testing.T) {
	if !IsLocalOverloadError(&store.AdmissionError{}) {
		t.Fatal("连接池准入错误应判为本地过载")
	}
	if !IsLocalOverloadError(ingress.ErrBodyBudgetExhausted) {
		t.Fatal("入站正文预算耗尽应判为本地过载")
	}
	if IsLocalOverloadError(dial.ErrConnect) {
		t.Fatal("建连失败不是本地过载")
	}
	if IsLocalOverloadError(nil) {
		t.Fatal("nil 不是本地过载")
	}
}

// TestFailureErrorMessage 断言失败文案不含密钥与正文。
func TestFailureErrorMessage(t *testing.T) {
	failure := &Failure{
		Category:     CategoryProviderError,
		StatusCode:   500,
		Message:      "internal error",
		Body:         `{"error":{"message":"internal error"}}`,
		ProviderID:   7,
		ProviderName: "供应商甲",
		Attempt:      2,
	}
	if got := failure.Error(); got != "forward: 供应商甲 返回 500（provider_error，第 2 次尝试）" {
		t.Fatalf("Error() = %q", got)
	}
	transport := &Failure{Category: CategorySystemError, Message: "dial: connect failed", Attempt: 1, Err: dial.ErrConnect}
	if got := transport.Error(); got != "forward: provider#0 失败（system_error，第 1 次尝试）：dial: connect failed" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(transport, dial.ErrConnect) {
		t.Fatal("Failure 必须保留底层错误链路")
	}
}
