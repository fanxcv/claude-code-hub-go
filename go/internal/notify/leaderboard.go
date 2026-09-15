package notify

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// DailyLeaderboard 生成日报数据（Node：tasks/daily-leaderboard.ts:11）。
//
// 口径：
//   - 榜单是「近 24 小时」的用户榜（全量按成本倒序，由 store 侧排序）；
//   - entries 只取前 topN 条（topN 由 LeaderboardTopN 折过缺省）；
//   - totalRequests / totalCost 是**全量**合计——收件人看到的「今日总量」不该等于前 N 名的和。
//
// 返回 (nil, nil) 表示本刻无数据（榜单为空）：调用方据此跳过投递，而不是发一份空榜。
func (g *Generators) DailyLeaderboard(
	ctx context.Context,
	topN int,
	systemTimezone string,
	now time.Time,
) (*DailyLeaderboardData, error) {
	if topN <= 0 {
		topN = DefaultLeaderboardTopN
	}
	location := Location(systemTimezone)
	// last24h 是滚动窗口（SQL 里是 CURRENT_TIMESTAMP - 24h），时区不参与——仍按 Node 传入。
	rows, err := g.Leaderboard.AdminUserLeaderboard(ctx, store.AdminLeaderboardQuery{
		Period:   "last24h",
		Timezone: systemTimezone,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	limit := topN
	if limit > len(rows) {
		limit = len(rows)
	}
	entries := make([]DailyLeaderboardEntry, 0, limit)
	var totalRequests, totalCost float64
	for index, row := range rows {
		cost := g.parseCostText("notify.leaderboard_cost_unparsable", row.TotalCostText)
		totalRequests += row.TotalRequests
		totalCost += cost
		if index >= limit {
			continue
		}
		entries = append(entries, DailyLeaderboardEntry{
			UserID:        row.UserID,
			UserName:      row.UserName,
			TotalRequests: row.TotalRequests,
			TotalCost:     cost,
			TotalTokens:   row.TotalTokens,
		})
	}

	return &DailyLeaderboardData{
		Date:          now.In(location).Format("2006-01-02"),
		Entries:       entries,
		TotalRequests: totalRequests,
		TotalCost:     totalCost,
	}, nil
}
