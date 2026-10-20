package store

import (
	"strings"
	"testing"
)

// 本文件钉住 outbox 保留期清理的**删除谓词形状**。
//
// 为何必须钉：这一条 SQL 是本仓唯一能真的删掉未发布事件的地方。谓词写错（漏掉
// `published_at IS NOT NULL`、写成 `IS NULL`、把 AND 改成 OR）没有任何运行期信号——
// 表面上「清理跑成功了，删了 N 行」，实际丢掉的是可用性投影的输入，且没有补偿路径
// （回填标记已 done）。所以用文本钉子把三个必要片段固定下来：任何改写只要动了判据就红。
//
// 谓词本身的**行为**由 jobs 包的真库用例（outbox_retention_integration_test.go）验证：
// 那里插「未发布但很旧」的行，跑一轮，断言它仍在。文本钉子只负责「不许悄悄改字」。
func TestDeletePublishedOutboxBeforeBatchSQLKeepsPublishedGuard(t *testing.T) {
	sql := deletePublishedOutboxBeforeBatchSQL

	required := []struct {
		fragment string
		why      string
	}{
		{"published_at IS NOT NULL", "未发布行是投影消费的唯一输入，谓词必须显式只命中已发布行"},
		{"published_at < $1", "保留期判据必须落在 published_at 上（不是 created_at：认领可能晚于落库很久）"},
		{"FOR UPDATE SKIP LOCKED", "与并发认领互不阻塞：没有它，清理会等锁或死锁"},
		{"ORDER BY id", "按主键升序取批，避免每批都从堆头重新扫"},
		{"LIMIT $2", "单批有界：不限量的 DELETE 会一次锁住整表并拉出巨量 WAL"},
		{"DELETE FROM outbox_events", "被删的只能是这张表"},
	}
	for _, item := range required {
		if !strings.Contains(sql, item.fragment) {
			t.Fatalf("删除语句缺少 %q（%s）:\n%s", item.fragment, item.why, sql)
		}
	}

	// 反面：不得出现「只删未发布」这类把谓词写反的形态。
	if strings.Contains(sql, "published_at IS NULL") {
		t.Fatalf("删除语句出现了 published_at IS NULL：那会删掉未发布事件，是永久数据缺口:\n%s", sql)
	}
	// 反面：不得删整表（TRUNCATE / 无条件 DELETE 都是事故）。
	if strings.Contains(strings.ToUpper(sql), "TRUNCATE") {
		t.Fatalf("删除语句不得 TRUNCATE:\n%s", sql)
	}
	if strings.Contains(sql, "WHERE true") || strings.Contains(sql, "WHERE 1=1") {
		t.Fatalf("删除语句出现了无条件 WHERE:\n%s", sql)
	}
}
