package notify

// 本文件是四份通知数据的形状，逐字段对齐 `src/lib/webhook/types.ts`。
//
// 为什么 struct 也放在本包而不是 adminapi：模板占位符（placeholders.ts）与绑定投递都按
// 这些 JSON 键取值，生成器与形状同处一包，改一处即改契约（src/types/notifications.ts 的
// 文件头也写着「改动这里的形状即改契约」）。

// DailyLeaderboardEntry 是日报里的一行。
type DailyLeaderboardEntry struct {
	UserID        int64   `json:"userId"`
	UserName      string  `json:"userName"`
	TotalRequests float64 `json:"totalRequests"`
	TotalCost     float64 `json:"totalCost"`
	TotalTokens   float64 `json:"totalTokens"`
}

// DailyLeaderboardData 是日报正文。
//
// TotalRequests / TotalCost 是**全量**合计（不是前 N 名的合计）。
type DailyLeaderboardData struct {
	Date          string                  `json:"date"`
	Entries       []DailyLeaderboardEntry `json:"entries"`
	TotalRequests float64                 `json:"totalRequests"`
	TotalCost     float64                 `json:"totalCost"`
}

// CostAlertData 是一份成本预警（一个对象一条告警：密钥的每个窗口各一条）。
type CostAlertData struct {
	TargetType  string  `json:"targetType"`
	TargetName  string  `json:"targetName"`
	TargetID    int64   `json:"targetId"`
	CurrentCost float64 `json:"currentCost"`
	QuotaLimit  float64 `json:"quotaLimit"`
	Threshold   float64 `json:"threshold"`
	Period      string  `json:"period"`
}

// CircuitBreakerAlertData 是熔断告警正文。
//
// 本包只提供**形状与去重键**：熔断事件由转发路径在开闸那一刻产生（Node 的
// sendCircuitBreakerAlert），Go 侧的对应产生点在数据面；生成器不参与事件型告警。
type CircuitBreakerAlertData struct {
	ProviderName   string `json:"providerName"`
	ProviderID     int64  `json:"providerId"`
	FailureCount   int    `json:"failureCount"`
	RetryAt        string `json:"retryAt"`
	LastError      string `json:"lastError,omitempty"`
	IncidentSource string `json:"incidentSource,omitempty"`
	EndpointID     int64  `json:"endpointId,omitempty"`
	EndpointURL    string `json:"endpointUrl,omitempty"`
}

// CacheHitRateAlertSample 是一个统计样本：窗口内按 kind 口径算出的命中率。
type CacheHitRateAlertSample struct {
	Kind              string  `json:"kind"`
	Requests          float64 `json:"requests"`
	DenominatorTokens float64 `json:"denominatorTokens"`
	HitRateTokens     float64 `json:"hitRateTokens"`
}

// CacheHitRateAlertAnomaly 是一条异常。
type CacheHitRateAlertAnomaly struct {
	ProviderID   int64  `json:"providerId"`
	ProviderName string `json:"providerName,omitempty"`
	ProviderType string `json:"providerType,omitempty"`
	Model        string `json:"model"`

	// BaselineSource 取 historical / today / prev；无可用基线时该条根本不会入选。
	BaselineSource string                   `json:"baselineSource"`
	Current        CacheHitRateAlertSample  `json:"current"`
	Baseline       *CacheHitRateAlertSample `json:"baseline"`

	DeltaAbs *float64 `json:"deltaAbs"`
	DeltaRel *float64 `json:"deltaRel"`
	DropAbs  *float64 `json:"dropAbs"`

	ReasonCodes []string `json:"reasonCodes"`
}

// CacheHitRateAlertWindow 是本次判定的窗口。
type CacheHitRateAlertWindow struct {
	Mode            string `json:"mode"`
	StartTime       string `json:"startTime"`
	EndTime         string `json:"endTime"`
	DurationMinutes int    `json:"durationMinutes"`
}

// CacheHitRateAlertSettingsSnapshot 是本次判定用到的**生效**设置（缺省值已折入）。
//
// 带上快照的理由：模板与排障需要知道「这条告警是按什么阈值判出来的」，而设置可能随后被改。
type CacheHitRateAlertSettingsSnapshot struct {
	WindowMode             string  `json:"windowMode"`
	CheckIntervalMinutes   int     `json:"checkIntervalMinutes"`
	HistoricalLookbackDays int     `json:"historicalLookbackDays"`
	MinEligibleRequests    int     `json:"minEligibleRequests"`
	MinEligibleTokens      int     `json:"minEligibleTokens"`
	AbsMin                 float64 `json:"absMin"`
	DropRel                float64 `json:"dropRel"`
	DropAbs                float64 `json:"dropAbs"`
	CooldownMinutes        int     `json:"cooldownMinutes"`
	TopN                   int     `json:"topN"`
}

// CacheHitRateAlertData 是缓存命中率告警正文。
type CacheHitRateAlertData struct {
	Window          CacheHitRateAlertWindow           `json:"window"`
	Anomalies       []CacheHitRateAlertAnomaly        `json:"anomalies"`
	SuppressedCount int                               `json:"suppressedCount"`
	Settings        CacheHitRateAlertSettingsSnapshot `json:"settings"`
	GeneratedAt     string                            `json:"generatedAt"`
}
