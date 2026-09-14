package providertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// engine.go 复刻 `src/lib/provider-testing/test-service.ts` 的执行编排：
// 计划生成（buildAttemptPlans）、逐计划尝试（executeProviderTest）、单次尝试
// （runSingleAttempt，含 versionless OpenAI URL 回退与代理）、错误归类（classifyError）。
//
// 与 Node 的分工差异（有意）：Node 的 `runSingleAttempt` 直接 fetch + AbortController；
// 本实现把「HTTP 执行面」抽成 `Doer` 注入，测试可换假上游，生产用 `DefaultClientFactory`
// （即探测面自己的 `NewUpstreamClient`，支持 http/https CONNECT 与 socks5）。

// 复刻 test-service.ts:39-41 的两个常量。
var (
	retryableHTTPStatusCodes = []int{400, 404, 405, 415, 422}
	invalidOpenAIURLMarker   = "Invalid URL (POST /v1/"
)

// 复刻 TEST_DEFAULTS（`types.ts:55-61`）：总超时 10s、慢阈值 5s。
const (
	defaultTestTimeoutMS    = 10000
	defaultSlowLatencyMS    = 5000
	minAttemptTimeoutMS     = 1000
	maxProviderTestRawBytes = 10 << 20
)

// StreamInfo 复刻 ProviderTestResult.streamInfo。
type StreamInfo struct {
	IsStreaming    bool `json:"isStreaming"`
	ChunksReceived int  `json:"chunksReceived,omitempty"`
}

// ValidationDetails 复刻 ValidationDetails（`types.ts`，六字段）。
type ValidationDetails struct {
	HTTPPassed     bool
	HTTPStatusCode *int
	LatencyPassed  bool
	LatencyMS      int64
	ContentPassed  bool
	ContentTarget  string
}

// Config 复刻 ProviderTestConfig（执行面用到的字段）。
type Config struct {
	ProviderID            string
	ProviderURL           string
	APIKey                string
	ProviderType          ProviderType
	Model                 string
	ProxyURL              string
	ProxyFallbackToDirect bool
	LatencyThresholdMS    *int64
	SuccessContains       string
	TimeoutMS             *int64
	Preset                string
	CustomPayload         string
	CustomHeaders         map[string]string
	GeminiBearerAuth      bool
	// Now 可注入时钟（nil 用 time.Now）。测延迟与 testedAt 用。
	Now func() time.Time
}

// AttemptPlan 复刻 AttemptPlan。
type AttemptPlan struct {
	Preset          *Preset
	Body            map[string]any
	Headers         map[string]string
	Model           string
	SuccessContains string
	URL             string
}

// Result 复刻 ProviderTestResult。
type Result struct {
	Success        bool
	Status         TestStatus
	SubStatus      TestSubStatus
	LatencyMS      int64
	FirstByteMS    *int64
	HTTPStatusCode *int
	HTTPStatusText string
	Model          string
	Content        string
	RawResponse    string
	RequestURL     string
	Usage          *TokenUsage
	StreamInfo     *StreamInfo
	ErrorMessage   string
	ErrorType      string
	TestedAt       time.Time
	Validation     ValidationDetails
	UsedProxy      bool
}

// Doer 是单次 HTTP 执行面（*http.Client 满足）。
type Doer interface {
	Do(request *http.Request) (*http.Response, error)
}

// ClientFactory 按代理地址与超时造 Doer；proxyURL 为空表示直连。
type ClientFactory func(proxyURL string, timeout time.Duration) (Doer, error)

// DefaultClientFactory 用探测面的上游客户端（`proxy.go` 的 NewUpstreamClient）。
func DefaultClientFactory(proxyURL string, timeout time.Duration) (Doer, error) {
	client, err := NewUpstreamClient(ProxyConfig{URL: proxyURL}, timeout)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("providertest: 上游客户端构造失败")
	}
	return client, nil
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) timeoutMS() int64 {
	if c.TimeoutMS != nil && *c.TimeoutMS > 0 {
		return *c.TimeoutMS
	}
	return defaultTestTimeoutMS
}

func (c Config) slowThresholdMS() int64 {
	if c.LatencyThresholdMS != nil {
		return *c.LatencyThresholdMS
	}
	return defaultSlowLatencyMS
}

func defaultSuccessContains(providerType ProviderType) string {
	return DefaultSuccessContains[providerType]
}

// mergeHeaders 复刻 Node 的对象展开顺序：base → custom 覆盖。
func mergeHeaders(base map[string]string, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

// BuildAttemptPlans 复刻 buildAttemptPlans（test-service.ts:46-121）。
//
// 三分支：自定义正文 → 单计划；指定 preset → 该 preset（不存在时报 `Preset not found`）；
// 否则按候选打分降序；候选为空时用协议级兜底体。
func BuildAttemptPlans(config Config) ([]AttemptPlan, error) {
	if custom := strings.TrimSpace(config.CustomPayload); custom != "" {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(custom), &parsed); err != nil {
			return nil, errors.New("Invalid custom payload JSON")
		}
		headers, err := TestHeaders(config.ProviderType, config.APIKey, config.ProviderURL, "", nil, config.GeminiBearerAuth)
		if err != nil {
			return nil, err
		}
		url, err := TestURL(config.ProviderURL, config.ProviderType, config.Model, "")
		if err != nil {
			return nil, err
		}
		contains := config.SuccessContains
		if contains == "" {
			contains = defaultSuccessContains(config.ProviderType)
		}
		return []AttemptPlan{{
			Body:            parsed,
			Headers:         mergeHeaders(headers, config.CustomHeaders),
			Model:           config.Model,
			SuccessContains: contains,
			URL:             url,
		}}, nil
	}

	var presets []Preset
	if config.Preset != "" {
		preset, ok := GetPreset(config.Preset)
		if !ok {
			return nil, fmt.Errorf("Preset not found: %s", config.Preset)
		}
		presets = []Preset{preset}
	} else {
		presets = ExecutionPresetCandidates(config.ProviderType, config.ProviderURL, config.Model)
	}

	if len(presets) == 0 {
		body, err := TestBody(config.ProviderType, config.Model)
		if err != nil {
			return nil, err
		}
		headers, err := TestHeaders(config.ProviderType, config.APIKey, config.ProviderURL, "", nil, config.GeminiBearerAuth)
		if err != nil {
			return nil, err
		}
		url, err := TestURL(config.ProviderURL, config.ProviderType, config.Model, "")
		if err != nil {
			return nil, err
		}
		contains := config.SuccessContains
		if contains == "" {
			contains = defaultSuccessContains(config.ProviderType)
		}
		return []AttemptPlan{{
			Body:            body,
			Headers:         mergeHeaders(headers, config.CustomHeaders),
			Model:           config.Model,
			SuccessContains: contains,
			URL:             url,
		}}, nil
	}

	plans := make([]AttemptPlan, 0, len(presets))
	for index := range presets {
		preset := presets[index]
		effectiveModel := config.Model
		if effectiveModel == "" {
			effectiveModel = preset.DefaultModel
		}
		body, err := PresetPayload(preset.ID, effectiveModel)
		if err != nil {
			return nil, err
		}
		headers, err := TestHeaders(
			config.ProviderType, config.APIKey, config.ProviderURL,
			preset.UserAgent, preset.ExtraHeaders, config.GeminiBearerAuth,
		)
		if err != nil {
			return nil, err
		}
		url, err := TestURL(config.ProviderURL, config.ProviderType, effectiveModel, preset.Path)
		if err != nil {
			return nil, err
		}
		contains := config.SuccessContains
		if contains == "" {
			contains = preset.DefaultSuccessContains
		}
		if contains == "" {
			contains = defaultSuccessContains(config.ProviderType)
		}
		plans = append(plans, AttemptPlan{
			Preset:          &preset,
			Body:            body,
			Headers:         mergeHeaders(headers, config.CustomHeaders),
			Model:           effectiveModel,
			SuccessContains: contains,
			URL:             url,
		})
	}
	return plans, nil
}

// ShouldRetryWithNextTemplate 复刻 shouldRetryWithNextTemplate。
func ShouldRetryWithNextTemplate(result Result) bool {
	if result.Status != StatusRed {
		return false
	}
	if result.HTTPStatusCode != nil && isRetryableHTTPStatus(*result.HTTPStatusCode) {
		return true
	}
	switch result.SubStatus {
	case SubStatusClientError, SubStatusInvalidRequest, SubStatusContentMismatch:
		return true
	default:
		return false
	}
}

func isRetryableHTTPStatus(status int) bool {
	for _, candidate := range retryableHTTPStatusCodes {
		if candidate == status {
			return true
		}
	}
	return false
}

func isOpenAIStyleProvider(providerType ProviderType) bool {
	return providerType == TypeCodex || providerType == TypeOpenAICompatible
}

func buildValidationDetails(
	responseStatus *int,
	latencyMS int64,
	slowThresholdMS int64,
	contentPassed bool,
	successContains string,
) ValidationDetails {
	passed := responseStatus != nil && *responseStatus >= 200 && *responseStatus < 300
	return ValidationDetails{
		HTTPPassed:     passed,
		HTTPStatusCode: responseStatus,
		LatencyPassed:  responseStatus != nil && latencyMS <= slowThresholdMS,
		LatencyMS:      latencyMS,
		ContentPassed:  contentPassed,
		ContentTarget:  successContains,
	}
}

// versionlessFallbackState 复刻 VersionlessFallbackState（跨尝试共享）。
type versionlessFallbackState struct {
	hasRetriedVersionlessURL bool
	preferVersionlessURL     bool
}

// resolveVersionlessOpenAIFallbackURL 复刻 resolveVersionlessOpenAiFallbackUrl。
func resolveVersionlessOpenAIFallbackURL(
	config Config,
	requestURL string,
	responseStatus int,
	responseBody string,
	state *versionlessFallbackState,
) string {
	if state.hasRetriedVersionlessURL {
		return ""
	}
	if !isOpenAIStyleProvider(config.ProviderType) {
		return ""
	}
	if responseStatus != 400 || !strings.Contains(responseBody, invalidOpenAIURLMarker) {
		return ""
	}
	return VersionlessOpenAIFallbackURL(requestURL)
}

// ExecuteProviderTest 复刻 executeProviderTest：逐计划尝试，命中即返回；
// 全部「可重试」失败时返回最后一次结果。
func ExecuteProviderTest(
	ctx context.Context,
	config Config,
	factory ClientFactory,
) (Result, error) {
	if factory == nil {
		factory = DefaultClientFactory
	}
	timeoutMS := config.timeoutMS()
	slowThresholdMS := config.slowThresholdMS()
	plans, err := BuildAttemptPlans(config)
	if err != nil {
		return Result{}, err
	}
	deadline := config.now().Add(time.Duration(timeoutMS) * time.Millisecond)
	state := &versionlessFallbackState{}

	var fallback *Result
	for _, plan := range plans {
		remaining := deadline.Sub(config.now()).Milliseconds()
		if remaining < minAttemptTimeoutMS {
			remaining = minAttemptTimeoutMS
		}
		result := runSingleAttempt(ctx, config, plan, time.Duration(remaining)*time.Millisecond, slowThresholdMS, state)
		if result.Success || !ShouldRetryWithNextTemplate(result) {
			return result, nil
		}
		copied := result
		fallback = &copied
	}
	if fallback != nil {
		return *fallback, nil
	}
	return Result{}, errors.New("No provider testing plan could be constructed")
}

// runSingleAttempt 复刻 runSingleAttempt（含 versionless 回退循环与代理透传）。
func runSingleAttempt(
	ctx context.Context,
	config Config,
	plan AttemptPlan,
	timeout time.Duration,
	slowThresholdMS int64,
	state *versionlessFallbackState,
) Result {
	startTime := config.now()
	requestURL := plan.URL
	if state.preferVersionlessURL && isOpenAIStyleProvider(config.ProviderType) {
		if fallback := VersionlessOpenAIFallbackURL(plan.URL); fallback != "" {
			requestURL = fallback
		}
	}
	usedProxy := false

	doer, err := factoryFor(config, plan, timeout)
	if err != nil {
		return networkFailure(config, plan, requestURL, startTime, slowThresholdMS, false, err)
	}
	if config.ProxyURL != "" {
		usedProxy = true
	}

	var firstByteMS *int64
	for {
		attemptStart := config.now()
		response, err := doProviderRequest(ctx, doer, plan, requestURL, timeout)
		if err != nil {
			return networkFailure(config, plan, requestURL, startTime, slowThresholdMS, usedProxy, err)
		}
		// firstByteMs 在**收到响应头**时取（Node 在 `await fetch` 之后、读正文之前）。
		firstByte := config.now().Sub(attemptStart).Milliseconds()
		firstByteMS = &firstByte
		body, readErr := readBody(response)
		if readErr != nil {
			return networkFailure(config, plan, requestURL, startTime, slowThresholdMS, usedProxy, readErr)
		}

		if fallbackURL := resolveVersionlessOpenAIFallbackURL(config, requestURL, response.StatusCode, body, state); fallbackURL != "" && fallbackURL != requestURL {
			requestURL = fallbackURL
			state.hasRetriedVersionlessURL = true
			state.preferVersionlessURL = true
			continue
		}

		latencyMS := config.now().Sub(startTime).Milliseconds()
		contentType := response.Header.Get("Content-Type")
		parsed := ParseResponse(config.ProviderType, body, contentType)
		validationInput := parsed.Content
		if validationInput == "" {
			validationInput = body
		}
		httpResult := ClassifyHTTPStatus(response.StatusCode, latencyMS, slowThresholdMS)
		contentResult := EvaluateContentValidation(httpResult.Status, httpResult.SubStatus, validationInput, plan.SuccessContains)

		statusCode := response.StatusCode
		content := parsed.Content
		if content == "" {
			content = body
		}
		result := Result{
			Success:        contentResult.Status != StatusRed,
			Status:         contentResult.Status,
			SubStatus:      contentResult.SubStatus,
			LatencyMS:      latencyMS,
			FirstByteMS:    firstByteMS,
			HTTPStatusCode: &statusCode,
			HTTPStatusText: response.Status,
			Model:          parsed.Model,
			Content:        content,
			RawResponse:    body,
			RequestURL:     requestURL,
			Usage:          parsed.Usage,
			TestedAt:       config.now(),
			UsedProxy:      usedProxy,
			Validation: buildValidationDetails(
				&statusCode, latencyMS, slowThresholdMS, contentResult.ContentPassed, plan.SuccessContains,
			),
		}
		if parsed.IsStreaming {
			result.StreamInfo = &StreamInfo{IsStreaming: true, ChunksReceived: parsed.ChunksReceived}
		}
		return result
	}
}

// factoryFor 按「是否配置代理」拿到本次尝试的执行面。
//
// 与 Node 的差异（登记）：Node 在 `runSingleAttempt` 里经 createProxyAgentForProvider 建
// dispatcher，代理建连失败会让该次请求失败（不回退直连，proxyFallbackToDirect 在 agent 层）；
// 本实现同判——代理配置了但建不出来就按网络错误返回，不静默直连。
func factoryFor(config Config, plan AttemptPlan, timeout time.Duration) (Doer, error) {
	if config.ProxyURL == "" {
		client, err := NewUpstreamClient(ProxyConfig{}, timeout)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	client, err := NewUpstreamClient(ProxyConfig{URL: config.ProxyURL, FallbackToDirect: config.ProxyFallbackToDirect}, timeout)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func doProviderRequest(
	ctx context.Context,
	doer Doer,
	plan AttemptPlan,
	requestURL string,
	timeout time.Duration,
) (*http.Response, error) {
	payload, err := json.Marshal(plan.Body)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	// cancel 由 Body 的 Close 负责（response 可能还在被读），这里用 AfterFunc 兜底超时。
	timer := time.AfterFunc(timeout, cancel)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	for key, value := range plan.Headers {
		request.Header.Set(key, value)
	}
	response, err := doer.Do(request)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	// 读到结束即释放定时器（正常路径）。
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, stop: func() { timer.Stop(); cancel() }}
	return response, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	stop func()
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	return err
}

func readBody(response *http.Response) (string, error) {
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxProviderTestRawBytes))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// networkFailure 复刻 runSingleAttempt 的 catch 分支（classifyError + validationDetails 全 false）。
func networkFailure(
	config Config,
	plan AttemptPlan,
	requestURL string,
	startTime time.Time,
	slowThresholdMS int64,
	usedProxy bool,
	err error,
) Result {
	latencyMS := config.now().Sub(startTime).Milliseconds()
	subStatus, errorType, errorMessage := ClassifyError(err)
	return Result{
		Success:      false,
		Status:       StatusRed,
		SubStatus:    subStatus,
		LatencyMS:    latencyMS,
		ErrorMessage: errorMessage,
		ErrorType:    errorType,
		RequestURL:   requestURL,
		TestedAt:     config.now(),
		UsedProxy:    usedProxy,
		Validation:   buildValidationDetails(nil, latencyMS, slowThresholdMS, false, plan.SuccessContains),
	}
}

// ClassifyError 复刻 classifyError（test-service.ts:378-452）。
func ClassifyError(err error) (TestSubStatus, string, string) {
	if err == nil {
		return SubStatusNetworkError, "unknown_error", "unknown error"
	}
	message := strings.ToLower(err.Error())

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		strings.Contains(message, "timeout") || strings.Contains(message, "timed out") ||
		strings.Contains(message, "context deadline exceeded") || strings.Contains(message, "aborted") {
		return SubStatusNetworkError, "timeout", "Request timed out"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) ||
		strings.Contains(message, "getaddrinfo") || strings.Contains(message, "enotfound") ||
		strings.Contains(message, "no such host") || strings.Contains(message, "dns") {
		return SubStatusNetworkError, "dns_error", "DNS resolution failed"
	}

	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(message, "econnrefused") ||
		strings.Contains(message, "connection refused") {
		return SubStatusNetworkError, "connection_refused", "Connection refused"
	}

	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(message, "econnreset") ||
		strings.Contains(message, "connection reset") {
		return SubStatusNetworkError, "connection_reset", "Connection reset by peer"
	}

	var recordHeaderErr tls.RecordHeaderError
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &recordHeaderErr) || errors.As(err, &certErr) ||
		strings.Contains(message, "ssl") || strings.Contains(message, "tls") ||
		strings.Contains(message, "certificate") {
		return SubStatusNetworkError, "ssl_error", "SSL/TLS error"
	}

	return SubStatusNetworkError, "network_error", err.Error()
}

// SubStatusMessages 复刻 SUB_STATUS_MESSAGES（actions/providers.ts:5024-5034）。
var SubStatusMessages = map[TestSubStatus]string{
	SubStatusSuccess:         "所有检查通过",
	SubStatusSlowLatency:     "响应成功但较慢",
	SubStatusRateLimit:       "请求被限流 (429)",
	SubStatusServerError:     "服务器错误 (5xx)",
	SubStatusClientError:     "客户端错误 (4xx)",
	SubStatusAuthError:       "认证失败 (401/403)",
	SubStatusInvalidRequest:  "无效请求 (400)",
	SubStatusNetworkError:    "网络连接失败",
	SubStatusContentMismatch: "响应内容验证失败",
}

// BuildUnifiedTestPayload 复刻 buildUnifiedTestSuccessData（actions/providers.ts:5128-5159）：
// 把内部结果映射成对外 payload（`testedAt` 为 ISO 毫秒串，缺省字段按 undefred 语义不出现）。
func BuildUnifiedTestPayload(result Result) map[string]any {
	statusText := ""
	switch result.Status {
	case StatusGreen:
		statusText = "可用"
	case StatusYellow:
		statusText = "波动"
	default:
		statusText = "不可用"
	}
	payload := map[string]any{
		"success":           result.Success,
		"status":            string(result.Status),
		"subStatus":         string(result.SubStatus),
		"message":           "供应商 " + statusText + ": " + SubStatusMessages[result.SubStatus],
		"latencyMs":         result.LatencyMS,
		"testedAt":          result.TestedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"validationDetails": buildValidationPayload(result.Validation),
	}
	if result.FirstByteMS != nil {
		payload["firstByteMs"] = *result.FirstByteMS
	}
	if result.HTTPStatusCode != nil {
		payload["httpStatusCode"] = *result.HTTPStatusCode
		payload["httpStatusText"] = result.HTTPStatusText
	}
	if result.Model != "" {
		payload["model"] = result.Model
	}
	if result.Content != "" {
		payload["content"] = result.Content
	}
	if result.RequestURL != "" {
		payload["requestUrl"] = result.RequestURL
	}
	if result.RawResponse != "" {
		payload["rawResponse"] = result.RawResponse
	}
	if result.Usage != nil {
		payload["usage"] = result.Usage
	}
	if result.StreamInfo != nil {
		payload["streamInfo"] = result.StreamInfo
	}
	if result.ErrorMessage != "" {
		payload["errorMessage"] = result.ErrorMessage
	}
	if result.ErrorType != "" {
		payload["errorType"] = result.ErrorType
	}
	return payload
}

func buildValidationPayload(details ValidationDetails) map[string]any {
	payload := map[string]any{
		"httpPassed":    details.HTTPPassed,
		"latencyPassed": details.LatencyPassed,
		"latencyMs":     details.LatencyMS,
		"contentPassed": details.ContentPassed,
		"contentTarget": details.ContentTarget,
	}
	if details.HTTPStatusCode != nil {
		payload["httpStatusCode"] = *details.HTTPStatusCode
	}
	return payload
}
