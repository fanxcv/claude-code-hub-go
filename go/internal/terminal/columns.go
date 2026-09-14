package terminal

// 本文件把「触发器要什么列」写成可断言的清单。它们不是从 TS 调用点抄来的——TS 里
// 看不到这些列——而是来自库内定义。

// ledgerMonitoredColumns 是 trg_upsert_usage_ledger 的 `UPDATE OF` 列表，实测 37 列。
// 其中任一项在终态写入时缺失或落错，账本行就会缺字段或算错 is_success。
var ledgerMonitoredColumns = []string{
	"blocked_by",
	"status_code",
	"error_message",
	"provider_chain",
	"actual_response_model",
	"endpoint",
	"provider_id",
	"user_id",
	"key",
	"model",
	"original_model",
	"api_type",
	"session_id",
	"session_identity",
	"session_identity_kind",
	"affinity_scope_tag",
	"affinity_fingerprint",
	"affinity_fingerprint_chain",
	"is_replay",
	"replay_source_request_id",
	"cost_usd",
	"cost_multiplier",
	"group_cost_multiplier",
	"input_tokens",
	"output_tokens",
	"cache_creation_input_tokens",
	"cache_read_input_tokens",
	"cache_creation_5m_input_tokens",
	"cache_creation_1h_input_tokens",
	"cache_ttl_applied",
	"context_1m_applied",
	"swap_cache_ttl_applied",
	"duration_ms",
	"ttfb_ms",
	"first_byte_ms",
	"client_ip",
	"created_at",
}

// outboxMonitoredColumns 是 message_request_outbox_aiud 的 `UPDATE OF` 列表（5 列）。
// 这 5 列决定 request_finalized 事件的产生时机与载荷完整度。
var outboxMonitoredColumns = []string{
	"status_code",
	"duration_ms",
	"error_message",
	"provider_chain",
	"blocked_by",
}

// createTimeColumns 是 store.CreateMessageRequest 的 INSERT 列集镜像。它由
// TestCreateTimeColumnsMatchGoldenRow 对着 go/testdata/golden/message_request_row.json
// 逐列校验——黄金样本来自真实 Node 写入的行，因此这份镜像一旦与真实行形状脱节就会红。
var createTimeColumns = []string{
	"provider_id",
	"user_id",
	"key",
	"model",
	"original_model",
	"duration_ms",
	"cost_usd",
	"cost_multiplier",
	"group_cost_multiplier",
	"session_id",
	"session_identity",
	"session_identity_kind",
	"affinity_scope_tag",
	"affinity_fingerprint",
	"affinity_fingerprint_chain",
	"is_replay",
	"replay_source_request_id",
	"request_sequence",
	"routing_trace",
	"user_agent",
	"client_ip",
	"endpoint",
	"messages_count",
	"special_settings",
	"cache_ttl_applied",
	"cache_creation_input_tokens",
	"cache_creation_5m_input_tokens",
	"cache_creation_1h_input_tokens",
	"cache_read_input_tokens",
}

// costTimeColumns 是成本写入路径能触达的监视列（store.UpdateCost / UpdateWinnerCost）。
var costTimeColumns = []string{"cost_usd", "cost_breakdown"}

// dbManagedColumns 是不经应用层显式赋值的监视列：created_at 由列默认值 now() 提供，
// 触发器读取的是行内已生效的值，因此不需要 Go 显式写。
var dbManagedColumns = []string{"created_at"}

// legacyColumns 是监视集里当前没有写入方的历史列：api_type 在黄金样本中为 NULL，
// TS 侧 createMessageRequest 也不写它。此处显式登记，避免「静默缺失」。
var legacyColumns = []string{"api_type"}
