package route

// 本文件回答用户 2026-09-22 的提问：「有多少请求因为撞上低速而被强制换渠道了呢？」
//
// 判据只读**已落下的选路留痕**（DecisionContext），不做任何新计算、不新增选路侧成本：
// 留痕本就在选路时产出，此处只是把它读成两个布尔。
//
// 两个成因必须分开（与 ReasonSlowRateCooldown / ReasonProviderErrorCooldown 分开同一个理由）：
// 一个是**会话级冷却**（该会话短期绕开这家），另一个是**渠道级降权**（排序被压下去）。
// 合并会把「这家被压了」与「这个会话刚被这家坑过」混为一谈。

// DivertCause 是「因低速被改道」的成因。取值是**存储契约**：已落下的桶按它求和，改名会让旧桶读不出来。
type DivertCause string

const (
	// DivertCauseCooldown 会话级冷却把该渠道剔出候选。
	DivertCauseCooldown DivertCause = "cooldown"
	// DivertCausePenalty 渠道级降权把该渠道挤出（它本会更优先）。
	DivertCausePenalty DivertCause = "penalty"
)

// Divert 是一次改道：某渠道因低速机制而没轮到，以及成因。
type Divert struct {
	ProviderID int64
	Cause      DivertCause
}

// DivertedAll 从一次选路的留痕里取全部「因低速被改道」的渠道。
//
// 返回零值（空切片）表示本次没有渠道被低速机制挤掉——可能压根没有候选、可能就是正常
// 加权落选、也可能是被熔断/限流等**非低速**理由剔除。这三种都**不该**计进改道数。
//
// 只认两个判据，且都用留痕里的既有字段：
//
//  1. 冷却：filteredProviders 里本家的 reason 是 slow_rate_cooldown。
//  2. 降权：本家在 consideredCandidates 里未选中、slowPenalty > 0，且**存在被选中者其
//     「未降权档位」比本家差（数值更大）**——即本家若非被降权就必被选中。
//
// 「未降权档位」即 `EffectivePriority - SlowPenalty`（降权是加在档位上的，见
// resolveEffectivePriority）。为何不用 ConsideredCandidate.Priority：那是 providers.priority
// 原值、**不含分组覆盖**，与真正参与排序的档位不同，拿它比较会在开了分组覆盖的渠道上误判。
//
// 为何要求「选中者的未降权档位**严格更差**」：只降权到**同档**时两家在同一档里参与加权
// 随机，本家即便不降权也只是「有机会」而非「必中」——那不是确定的改道，计进去就是编造。
//
// 一趟扫描完成（不逐家重扫候选），因为本函数在终态旁路上跑：留痕最多几十家，
// 逐家调一次单家判据会变成 O(n²)。
func DivertedAll(context DecisionContext) []Divert {
	out := make([]Divert, 0, len(context.FilteredProviders))
	for _, filtered := range context.FilteredProviders {
		if filtered.Reason == ReasonSlowRateCooldown {
			out = append(out, Divert{ProviderID: filtered.ID, Cause: DivertCauseCooldown})
		}
	}
	winnerBase, winnerIndex, hasWinner := 0, -1, false
	for index := range context.ConsideredCandidates {
		if context.ConsideredCandidates[index].Selected {
			winner := context.ConsideredCandidates[index]
			winnerBase = winner.EffectivePriority - winner.SlowPenalty
			winnerIndex = index
			hasWinner = true
			break
		}
	}
	if !hasWinner {
		return out
	}
	for index := range context.ConsideredCandidates {
		candidate := context.ConsideredCandidates[index]
		if candidate.Selected || candidate.SlowPenalty <= 0 {
			continue
		}
		// 两半都得成立，才是「只因降权落选」：
		//  ① 降权后确实落到了更差的一档（否则它仍在同一档参与加权随机，只是没抽中）；
		//  ② 若不降权则严格更优（否则即便不降权也轮不上它）。
		if candidate.EffectivePriority <= context.ConsideredCandidates[winnerIndex].EffectivePriority {
			continue
		}
		if candidate.EffectivePriority-candidate.SlowPenalty < winnerBase {
			out = append(out, Divert{ProviderID: candidate.ID, Cause: DivertCausePenalty})
		}
	}
	return out
}
