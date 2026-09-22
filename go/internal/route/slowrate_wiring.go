package route

import "context"

// 本文件把 SlowRateReader 的两次只读接到选路请求上：渠道级降权表与会话级冷却集。
//
// 分开两处调用而不是合并成一次：两者的 key 空间不同（`cch:slow:*` 与 `session-binding:v1:*`）、
// 输入不同（前者要模型，后者要会话身份）、装配开关也不同（前者看 SlowRate，后者还要有 SessionID）。
// 合并会让「无会话时也要按渠道读一遍」这种无谓往返变成默认行为。

// slowRatePenalties 取本场选路的渠道级降权表。
//
// 不装配读取器、未给模型、无开启监控的渠道时返回 nil（nil 与空表同义，见 penaltyTable）。
func (s *Selector) slowRatePenalties(
	ctx context.Context,
	providers []Provider,
	req Request,
) penaltyTable {
	if s.opts.SlowRate == nil {
		return nil
	}
	penalties := s.opts.SlowRate.Penalties(ctx, providers, req.Model)
	if len(penalties) == 0 {
		return nil
	}
	return penaltyTable(penalties)
}

// slowRateCooldown 取「本会话正在冷却中」的渠道及其成因。
//
// 无会话身份（req.SessionID 空 或 KeyID 为 0）时返回 nil：冷却键的形制含会话与 key，
// 缺任一项都构不出键，也就没有冷却可言。
//
// 返回成因而不是 bool：同一个冷却键的两个写入者（故障回避 / 低速降权）要记不同理由。
func (s *Selector) slowRateCooldown(
	ctx context.Context,
	providers []Provider,
	req Request,
) map[int64]CooldownKind {
	if s.opts.SlowRate == nil || req.SessionID == "" || req.KeyID == 0 {
		return nil
	}
	cooldown := s.opts.SlowRate.InCooldown(ctx, req.SessionID, req.KeyID, providers)
	if len(cooldown) == 0 {
		return nil
	}
	return cooldown
}
