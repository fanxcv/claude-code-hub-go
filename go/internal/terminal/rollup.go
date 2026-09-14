package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 public-status 投影的**事件捕获旁路**：终态提交之后，把这一次请求折算成 rollup 增量。
//
// Node 对应物：`src/repository/message.ts:246` 的 `queuePublicStatusRollupWrite`，
// 它位于终态收尾链里（`updateMessageRequestDetails` 的 `.then()` 段），与计费落库同一批副作用。
//
// 三条设计约束（每条都有对应的注释与测试）：
//
//  1. **失败不得影响结算**：rollup 是旁路统计，不是账务。任何失败（回读失败、Redis 不可用、
//     分组未配置…）只记 warn 并返回，绝不冒泡成结算错误。反证见
//     `TestSettleRollupFailureDoesNotAffectSettlement`（人为让 rollup 报错 → 结算仍成功、只多一条 warn）。
//
//  2. **时机：终态提交之后**。只在 `Result.Committed` 为真时写。这同时解决**重复计数**：
//     终态写由库内谓词（`UpdateDetailsIfUnfinalized`）保证单一赢家，重复结算会拿到
//     `Committed=false` → 不写第二次。Node 用「claim/unclaim + seed 注册表」达到同一目的，
//     而 Go 的终态屏障本身就是那个闸，故不需要再叠一层进程内注册表。
//
//  3. **不依赖请求生命周期**：用 `context.WithoutCancel` 派生带超时的上下文。请求在终态之后
//     可能立刻被客户端断开或进入排空，若沿用原 ctx，旁路会被顺带取消——那会让「刚成功的一次
//     请求」在公开页上消失。Node 侧同样脱离请求：它挂在行更新的 promise 链上，失败只记日志。

// RollupRecorder 是 public-status 投影的事件接收面。
//
// 用窄接口而不是直接要求 `*pubstatus.RollupRecorder`：terminal 只负责「把终态事实交出去」，
// 不关心对方是 Redis 写入、内存队列还是 nil（未装配）。
type RollupRecorder interface {
	// RecordTerminal 记录一次终态请求。**不得**返回错误——实现必须自带降级与日志；
	// 返回值留空是刻意的：调用方没有可做的补救（旁路失败不影响账务）。
	RecordTerminal(ctx context.Context, event pubstatus.RollupEvent)
}

// Logger 是旁路记 warn 的最小日志面（`logx.Logger` 满足）。
//
// 为什么 terminal 到这里才需要日志：旁路的失败**不得**冒泡（见文件头第 1 条），
// 只能落日志；而结算本身没有别的失败需要记（错误都是返回值）。
type Logger interface {
	Warn(event string, fields map[string]any)
}

// warn 在未装配日志时静默。
func (s *Settler) warn(event string, fields map[string]any) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(event, fields)
}

// RollupFactsReader 是可选的读取面：装配了 Rollup 旁路的 Writer 需要实现它。
//
// 单独一个接口而不是并进 Writer：见 writer.go 里 FindRollupFacts 的注释。
type RollupFactsReader interface {
	FindRollupFacts(ctx context.Context, id int64) (*store.RollupFacts, error)
}

// rollupTimeout 是旁路写入的上界。
//
// 取 3 秒：Redis 的默认命令超时量级、远小于请求超时；即使 Redis 半死，也只让该请求的收尾
// 多等这一小段，不会把结算拖成超时（结算本身已经完成，这里是它之后的旁路）。
const rollupTimeout = 3 * time.Second

// rollupChainItem 是 `provider_chain` 列里分类需要的字段。
//
// 只声明分类器真正会读的四个：列里还有选路留痕（priority/weight/decisionContext…），
// 那些进不了结局判定，多声明只会让人以为它们参与分类。
type rollupChainItem struct {
	StatusCode   *int    `json:"statusCode"`
	Reason       *string `json:"reason"`
	ErrorMessage *string `json:"errorMessage"`
	GroupTag     *string `json:"groupTag"`
	ErrorDetails *struct {
		MatchedRule any `json:"matchedRule"`
	} `json:"errorDetails"`
}

// buildRollupEvent 把「行事实 + 结算载荷」折算成一次 rollup 事件。
//
// 字段来源逐条对齐 Node 的 message.ts:246 调用点：
//
//	| 事件字段 | Node | 本实现 |
//	| createdAt | seed.createdAt（行创建时刻） | facts.CreatedAt |
//	| model | details.model ?? seed.model（终态给的响应模型优先） | settlement.ActualResponseModel ?? facts.Model |
//	| originalModel | seed.originalModel | facts.OriginalModel |
//	| durationMs | seed.durationMs | settlement.DurationMS ?? facts.DurationMS |
//	| ttftMs / firstByteMs / outputTokens / providerChain | details.* | settlement 同名字段 |
func buildRollupEvent(
	facts *store.RollupFacts,
	settlement Settlement,
) pubstatus.RollupEvent {
	event := pubstatus.RollupEvent{
		CreatedAt:     facts.CreatedAt,
		OriginalModel: facts.OriginalModel,
		Model:         facts.Model,
		ProviderChain: decodeRollupChain(settlement.ProviderChain, settlement.ErrorMessage),
	}
	if settlement.ActualResponseModel != nil && *settlement.ActualResponseModel != "" {
		event.Model = settlement.ActualResponseModel
	}
	if settlement.DurationMS != nil {
		event.DurationMs = float64Pointer(float64(*settlement.DurationMS))
	} else if facts.DurationMS != nil {
		event.DurationMs = float64Pointer(float64(*facts.DurationMS))
	}
	if settlement.TTFTMS != nil {
		event.TTFTMs = float64Pointer(float64(*settlement.TTFTMS))
	}
	if settlement.FirstByteMS != nil {
		event.FirstByteMs = float64Pointer(float64(*settlement.FirstByteMS))
	}
	if settlement.Usage.OutputTokens != nil {
		event.OutputTokens = settlement.Usage.OutputTokens
	}
	return event
}

// rollupReasonAliases 把**落链词汇**归一成 Node 的 `ProviderChainItem["reason"]` 取值域。
//
// 为什么需要它（实测发现，见报告）：Go 数据面写进 `provider_chain` 的 reason 与 Node 的取值域
// 有三处不同，而公开状态的结局判定（以及任何按 Node 词表读链的消费者）只认 Node 那一套：
//
//	| Go 落链 | Node 取值域 | 后果（若不归一） |
//	| --- | --- | --- |
//	| `success` | `request_success` | **每次成功都被算成失败**——可用率恒 0，最致命的一处 |
//	| `non_retryable_client_error` | `client_error_non_retryable` | 该被排除（local_non_retryable）的请求被算成失败 |
//	| `local_overload` | `concurrent_limit_failed` | 该被排除（local_capacity）的请求被算成失败 |
//
// 归一放在**本旁路**而不是改 `internal/forward`：改落链词汇会影响所有既有读者与钉子
// （那是跨包整理，超出本波授权）。这条差异已登记在报告里，建议后续在写入侧对齐。
var rollupReasonAliases = map[string]string{
	"success":                    "request_success",
	"non_retryable_client_error": "client_error_non_retryable",
	"local_overload":             "concurrent_limit_failed",
}

// normalizeRollupReason 按上面的别名表归一；未知值原样返回（分类器会按「未知 reason → 失败」处理，
// 与 Node 对未知 reason 的判定一致）。
func normalizeRollupReason(reason string) string {
	if mapped, ok := rollupReasonAliases[reason]; ok {
		return mapped
	}
	return reason
}

// decodeRollupChain 把落库的 `provider_chain` 解成分类器输入。
//
// 两处与 Node 的**登记差异**（因为 Go 落链的字段比 Node 少，见 `internal/route/chain.go` 的
// ChainItem：只有 id/name/reason/groupTag 等，没有 statusCode/errorMessage/errorDetails）：
//
//  1. `statusCode` 缺失 → 分类器里「按状态码判定」的几支（2xx 成功、404、499）在这些链项上
//     不会触发；但 `reason` 恒有（尝试结局），故成功/失败/排除的主干判定仍按 Node 的语义走。
//  2. `errorMessage` 缺失 → 「无可用供应商」「配额/限流」这两支**按文案**的排除会失效。
//     故这里用**结算载荷的 `ErrorMessage`** 兜底填进链项：它记的正是同一次请求的失败文案，
//     比直接丢字段更接近 Node 的输入。
func decodeRollupChain(raw []byte, fallbackErrorMessage *string) []pubstatus.ProviderChainItem {
	if len(raw) == 0 {
		return nil
	}
	var items []rollupChainItem
	if err := json.Unmarshal(raw, &items); err != nil {
		// 链解析失败不该让旁路崩：返回 nil 表示「没有可判定项」，事件照常交给上层
		// （增量会为空 → 上层按 ignored 处理）。
		return nil
	}
	out := make([]pubstatus.ProviderChainItem, 0, len(items))
	for index := range items {
		item := items[index]
		entry := pubstatus.ProviderChainItem{
			StatusCode: item.StatusCode,
			GroupTag:   item.GroupTag,
		}
		if item.Reason != nil {
			normalized := normalizeRollupReason(*item.Reason)
			entry.Reason = &normalized
		}
		if item.ErrorMessage != nil {
			entry.ErrorMessage = item.ErrorMessage
		} else if index == len(items)-1 {
			// 只有**最后一项**（本次请求的结局）才吃结算文案：前面的尝试各有各的失败文案，
			// 把结局文案贴到它们身上会凭空造出「无可用供应商」式的排除。
			entry.ErrorMessage = fallbackErrorMessage
		}
		if item.ErrorDetails != nil {
			entry.ErrorDetails = &pubstatus.ProviderChainErrorDetails{MatchedRule: item.ErrorDetails.MatchedRule}
		}
		out = append(out, entry)
	}
	return out
}

// recordRollup 是旁路的唯一执行点：**永不返回错误**。
func (s *Settler) recordRollup(ctx context.Context, id int64, settlement Settlement) {
	if s.rollup == nil {
		return
	}
	reader, ok := s.writer.(RollupFactsReader)
	if !ok {
		// Writer 没实现读取面：旁路整段跳过（装配不完整时静默，与「未装配 Rollup」同判）。
		return
	}

	// 脱离请求生命周期（见文件头第 3 条）。
	rollupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollupTimeout)
	defer cancel()

	facts, err := reader.FindRollupFacts(rollupCtx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.warn("terminal.rollup_facts_failed", map[string]any{"rowId": id, "error": err.Error()})
		}
		// ErrNotFound 属正常情形（行被软删、或预热抢答行）——两者 Node 都不计入公开统计。
		return
	}
	if facts == nil {
		return
	}
	s.rollup.RecordTerminal(rollupCtx, buildRollupEvent(facts, settlement))
}

func float64Pointer(value float64) *float64 { return &value }
