package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestIntegrationGroupPrioritiesDirtyRowDoesNotPoisonBatch 是容错读取的**核心反证**：
// 一行的 group_priorities 是脏值（顶层标量 / 数组 / 字符串数字）时，同批其它供应商必须照常可用。
//
// 为什么这条必须有：只读路径是 row_to_json 的**逐行**解码，readRowsAs 在第一个解不开的行上
// 直接返回错误；而 providers 是所有数据面请求的候选来源——一行脏值 ⇒ 整批读取失败 ⇒
// 选路拿到 0 家候选 ⇒ 全站 503，且日志里只看到「反序列化只读行失败」，看不出是哪一家、哪一列。
// 生产库的 group_priorities 写入侧历史上**无形状校验**，这种脏值确实写得进去。
//
// 用例自带清理：不把夹具留在共享库上（同库还有别的 lane 在跑）。干净值的解码正确性
// （vip=0/default=5 等）由 read_integration_test.go 覆盖，这里只压「脏值不扩散」。
func TestIntegrationGroupPrioritiesDirtyRowDoesNotPoisonBatch(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	marker := itKey(t)

	insert := func(label string, priorities string) int64 {
		t.Helper()
		var id int64
		// group_priorities 走 jsonb 原文写入：这正是「写入侧无形状校验」的复现路径。
		if err := pool.QueryRow(ctx, `
			INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority,
				group_tag, group_priorities)
			VALUES ($1, 'http://127.0.0.1:9', 'upstream-not-used', 'codex', true, 1, 0, 'default', $2::jsonb)
			RETURNING id`, "gp-dirty-"+label+"-"+marker, priorities).Scan(&id); err != nil {
			t.Fatalf("建供应商 %s 失败: %v", label, err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM providers WHERE id = $1`, id); err != nil {
				t.Errorf("清理供应商 %d 失败: %v", id, err)
			}
		})
		return id
	}

	cleanID := insert("clean", `{"fan": 0, "vip": 2}`)
	scalarID := insert("scalar", `"not-an-object"`)
	arrayID := insert("array", `[1, 2]`)
	stringValueID := insert("string-value", `{"fan": "0"}`)

	wanted := map[int64]bool{cleanID: true, scalarID: true, arrayID: true, stringValueID: true}

	providers, err := pools.FindEnabledProviders(ctx)
	if err != nil {
		t.Fatalf("脏值行不得让整批供应商读取失败: %v", err)
	}
	found := map[int64]Provider{}
	for _, provider := range providers {
		if wanted[provider.ID] {
			found[provider.ID] = provider
		}
	}
	if len(found) != len(wanted) {
		t.Fatalf("应有 %d 行夹具同行返回，实际 %d 行（整批 %d 行）",
			len(wanted), len(found), len(providers))
	}

	// 干净行：覆盖可用、无问题。
	clean := found[cleanID]
	cleanPriorities, cleanIssues := DecodeGroupPriorities(clean.GroupPriorities)
	if len(cleanIssues) != 0 {
		t.Fatalf("干净行不该报问题: %+v", cleanIssues)
	}
	if cleanPriorities["fan"] != 0 || cleanPriorities["vip"] != 2 {
		t.Fatalf("干净行的覆盖 = %v, want fan=0 vip=2", cleanPriorities)
	}

	// 脏行：一条覆盖都取不出，且必须**报出问题**（不静默）。
	dirtyCases := []struct {
		id               int64
		label            string
		wantTopLevel     bool
		wantIssueKeyHint string
	}{
		{id: scalarID, label: "顶层标量", wantTopLevel: true},
		{id: arrayID, label: "顶层数组", wantTopLevel: true},
		{id: stringValueID, label: "字符串值", wantIssueKeyHint: "fan"},
	}
	for _, testCase := range dirtyCases {
		provider := found[testCase.id]
		decoded, issues := DecodeGroupPriorities(provider.GroupPriorities)
		if len(decoded) != 0 {
			t.Fatalf("%s：不该解出任何覆盖，实际 %v", testCase.label, decoded)
		}
		if len(issues) == 0 {
			t.Fatalf("%s：必须报出问题（静默丢弃正是本次要消除的行为）", testCase.label)
		}
		for _, issue := range issues {
			if issue.JSONType == "" {
				t.Fatalf("%s：问题必须带 jsonb 类型: %+v", testCase.label, issue)
			}
			if issue.TopLevel != testCase.wantTopLevel {
				t.Fatalf("%s：topLevel = %v，期望 %v", testCase.label, issue.TopLevel, testCase.wantTopLevel)
			}
			if testCase.wantIssueKeyHint != "" && issue.Key != testCase.wantIssueKeyHint {
				t.Fatalf("%s：问题键 = %q，期望 %q", testCase.label, issue.Key, testCase.wantIssueKeyHint)
			}
		}
	}

	// 反证：把**同一份** row_to_json 载荷按旧形状（map[string]int）解——修前就是这样解的，
	// 它必然解不开（readRowsAs 于是整批返回错误）。若这里解得开，说明脏值形态选错了，
	// 这条用例就失去了证明力，故必须显式断言它失败。
	for _, id := range []int64{scalarID, arrayID, stringValueID} {
		payload := rawProviderPayload(t, ctx, pool, id)
		var legacyRow struct {
			GroupPriorities map[string]int `json:"group_priorities"`
		}
		if err := json.Unmarshal([]byte(payload), &legacyRow); err == nil {
			t.Fatalf("供应商 %d：旧形状（map[string]int）本应解不开这份载荷，实际解成 %v",
				id, legacyRow.GroupPriorities)
		}
	}

	// 反证（**批次级**）：修前 readRowsAs 在第一个解不开的行上直接返回错误，于是**整批**供应商
	// 都读不出来（不是只有脏那一家）。这里用与 FindEnabledProviders **逐字相同**的 SQL 取回
	// 每行载荷，按旧形状逐行解一遍，确认这批里确实存在解不开的行——即修前这里必定整批失败。
	rows, err := pool.Query(ctx, `
		SELECT row_to_json(t)::text FROM (
			SELECT * FROM providers WHERE is_enabled = true AND deleted_at IS NULL
			ORDER BY priority ASC, id ASC
		) t`)
	if err != nil {
		t.Fatalf("取整批载荷失败: %v", err)
	}
	defer rows.Close()
	undecodable := 0
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("读整批载荷失败: %v", err)
		}
		var legacyRow struct {
			GroupPriorities map[string]int `json:"group_priorities"`
		}
		if err := json.Unmarshal([]byte(payload), &legacyRow); err != nil {
			undecodable++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历整批载荷失败: %v", err)
	}
	if undecodable < len(dirtyCases) {
		t.Fatalf("本批应有至少 %d 行按旧形状解不开（本用例的脏夹具），实际 %d 行——\n"+
			"若为 0，则本反证失去证明力，说明脏值形态选错或夹具没写进去",
			len(dirtyCases), undecodable)
	}

	// 干净行按旧形状仍应解得出——说明修复没有把「可用的覆盖」也一并放宽。
	cleanPayload := rawProviderPayload(t, ctx, pool, cleanID)
	var cleanLegacyRow struct {
		GroupPriorities map[string]int `json:"group_priorities"`
	}
	if err := json.Unmarshal([]byte(cleanPayload), &cleanLegacyRow); err != nil {
		t.Fatalf("干净行按旧形状应解得开: %v", err)
	}
	if cleanLegacyRow.GroupPriorities["vip"] != 2 {
		t.Fatalf("干净行按旧形状解出的覆盖 = %v", cleanLegacyRow.GroupPriorities)
	}
}

// rawProviderPayload 取某个供应商在只读路径里**实际收到的**那份 JSON 文本，
// 供反证用例按旧形状复现修前的解码。
func rawProviderPayload(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64) string {
	t.Helper()
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM providers WHERE id = $1) t`, id,
	).Scan(&payload); err != nil {
		t.Fatalf("取供应商 %d 的只读载荷失败: %v", id, err)
	}
	return payload
}
