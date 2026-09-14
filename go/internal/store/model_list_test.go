package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// TestModelListProviderRowsIntegration 验证聚合模型列表的只读视图与库 schema 对齐。
//
// 为什么必须有：列名/表达式写错时，单测（用假目录）全绿而生产一调就 500。
// 本用例真连库跑一次 SELECT，把「SQL 成立」这件事钉住。
func TestModelListProviderRowsIntegration(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pools, err := Open(ctx, Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-model-list-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	defer func() { _ = pools.Close() }()

	rows, err := pools.ModelListProviderRows(ctx)
	if err != nil {
		t.Fatalf("读取供应商行失败（可能是列名与 schema 不符）: %v", err)
	}
	t.Logf("启用态供应商行数: %d", len(rows))
	for _, row := range rows {
		if row.ID == 0 || row.ProviderType == "" {
			t.Fatalf("行缺少 id/provider_type: %+v", row)
		}
		if row.URL == "" {
			// URL 为空是数据问题，不是本查询的问题；只提示，不判失败。
			t.Logf("供应商 %d(%s) 的 url 为空", row.ID, row.Name)
		}
	}

	// 有效分组：取库里一条真实密钥与用户，断言查询成立（返回值可为空串）。
	reader, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var keyID int64
	var userID int64
	if err := reader.QueryRow(ctx, `SELECT id, user_id FROM keys ORDER BY id ASC LIMIT 1`).Scan(&keyID, &userID); err != nil {
		t.Skipf("库里没有密钥，跳过有效分组用例: %v", err)
	}
	group, err := pools.ModelListEffectiveGroup(ctx, keyID, userID)
	if err != nil {
		t.Fatalf("读取有效分组失败: %v", err)
	}
	t.Logf("keyID=%d userID=%d 的有效分组=%q", keyID, userID, group)

	// 时区解析：不报错且非空（活动时段判定依赖它）。
	if tz := pools.AdminSystemTimezoneOrUTC(ctx); tz == "" {
		t.Fatal("系统时区不应为空")
	}
}
