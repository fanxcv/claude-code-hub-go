package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是「链项尝试结局细节」的真实依赖集成测试（env 门控：未设置 CCH_TEST_DSN 时跳过）。
//
// 单测（chain_details_test.go）只证明**编码器**会写这三个键；本文件证明它们能经真实
// SQL 落到 `message_request.provider_chain`，并且**取值与该次尝试的真实结局一致**：
//   - 成功条目：statusCode = 上游真实状态码；
//   - 配了重定向规则的供应商：modelRedirect 有原模型→转发模型与命中规则；
//   - 上游报错：statusCode = 上游错误码 + 非空 errorMessage。
//
// 为什么必须在真库上验：这三个键此前从不落库，前端三处恒空白；而「写进结构体」到
// 「出现在行里」之间还隔着 JSON 编码、列更新与终态屏障三道环节。

// setProviderModelRedirects 给夹具供应商写入重定向规则（与 Node 的
// provider-model-redirects 同形）。
func setProviderModelRedirects(t *testing.T, pools *store.Pools, providerID int64, rules string) {
	t.Helper()
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := writer.Exec(context.Background(),
		`UPDATE providers SET model_redirects = $2::jsonb WHERE id = $1`,
		providerID, rules,
	); err != nil {
		t.Fatalf("写入重定向规则失败: %v", err)
	}
}

// chainEntryFor 取链上属于该供应商的第一条条目。
// chainEntryFor 取链上该供应商的**尝试期**条目。
//
// 为何不能直接返回首个匹配项：链首现在是**选择期**条目（initial_selection / affinity_hit /
// session_reuse，见 `route.Result.ChainItem` 与 `dataplane.selectionChainEntry`），它与后续尝试项
// 共用同一个供应商 id，但按 Node 的语义**不写** statusCode / errorMessage / modelRedirect。
// 本文件的断言针对「一次真实 HTTP 交换的结局」，故优先取带 statusCode 的那一项；
// 都没有（本地拒绝类条目）时退回首个匹配项。
func chainEntryFor(t *testing.T, chain []map[string]any, providerID int64) map[string]any {
	t.Helper()
	var fallback map[string]any
	for _, item := range chain {
		id, ok := item["id"].(float64)
		if !ok || int64(id) != providerID {
			continue
		}
		if _, hasStatus := item["statusCode"]; hasStatus {
			return item
		}
		if fallback == nil {
			fallback = item
		}
	}
	if fallback != nil {
		return fallback
	}
	t.Fatalf("链上没有供应商 %d 的条目：%v", providerID, chain)
	return nil
}

// latestRowContainingProvider 取「链里含该夹具供应商」的**本次请求**落库的终态行。
//
// 为何不能按 `provider_id` 找行（像其它夹具那样）：本文件的失败用例里上游恒返 500，
// 网关会**故障转移到库里的其它供应商**，于是落库行的 provider_id 未必是夹具那家；
// 但该行仍属**同一次请求**，链上一定含夹具供应商的失败条目。
//
// 取行三原则：
//  1. **夹具身份**：链里必须含本用例的夹具供应商 id。用 jsonb 包含（`@>`）做精确匹配，
//     不会把 156 误配成 1561；链是对象而非数组的行自然也不包含。
//  2. **水位**：只看夹具建立之后落库的行（`id > watermark`）。共享测试库里有别的包留下的行
//     （例如熔断日志夹具插的 `provider_chain = {"id":-1}`），不设水位就会把它们当成自己的请求。
//  3. **形状守卫**：provider_chain 不是「对象数组」时**跳过该行**，而不是 Fatal——别人的夹具行
//     不得让本用例假红；但一条都找不到时仍按超时失败，不会静默变绿。
func latestRowContainingProvider(t *testing.T, provisioned provisioning, providerID int64) map[string]any {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows, queryErr := reader.Query(context.Background(), `
			SELECT row_to_json(t)::text FROM (
				SELECT * FROM message_request
				WHERE user_id = $1
				  AND id > $2
				  AND provider_chain @> jsonb_build_array(jsonb_build_object('id', $3::bigint))
				ORDER BY id DESC LIMIT 8
			) t`, provisioned.userID, provisioned.watermark, providerID)
		if queryErr != nil {
			t.Fatalf("查请求行失败: %v", queryErr)
		}
		var payloads []string
		for rows.Next() {
			var payload string
			if scanErr := rows.Scan(&payload); scanErr != nil {
				rows.Close()
				t.Fatalf("扫描请求行失败: %v", scanErr)
			}
			payloads = append(payloads, payload)
		}
		rows.Close()
		for _, payload := range payloads {
			var row map[string]any
			if unmarshalErr := json.Unmarshal([]byte(payload), &row); unmarshalErr != nil {
				continue
			}
			if row["status_code"] == nil {
				continue
			}
			chain, shapeOK := decodeObjectArrayOrNil(row["provider_chain"])
			if !shapeOK {
				// 形状不符：不是本用例的链（别人的夹具行），跳过而不是 Fatal。
				continue
			}
			for _, item := range chain {
				if id, ok := item["id"].(float64); ok && int64(id) == providerID {
					return row
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("20s 内未找到链中含供应商 %d 的终态行", providerID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestIntegrationStreamChainCarriesStatusCodeAndRedirect(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	const redirectTarget = "claude-sonnet-4-5-20250929"
	setProviderModelRedirects(t, pools, provisioned.providerID,
		fmt.Sprintf(`[{"matchType":"exact","source":%q,"target":%q}]`, integrationModel, redirectTarget))

	status, _ := integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}

	row := waitForFinalizedRow(t, provisioned)
	chain := providerChainOf(t, row)
	if len(chain) == 0 {
		t.Fatalf("流式请求的 provider_chain 不应为空：%v", row["provider_chain"])
	}
	item := chainEntryFor(t, chain, provisioned.providerID)

	// ① 成功条目必须带真实状态码（此前该键从不落库）。
	code, ok := item["statusCode"].(float64)
	if !ok || int(code) != http.StatusOK {
		t.Fatalf("成功条目 statusCode = %v，期望 200：%v", item["statusCode"], item)
	}
	// 成功条目不应有错误信息。
	if msg, present := item["errorMessage"]; present && msg != nil && msg != "" {
		t.Fatalf("成功条目不应有 errorMessage：%v", msg)
	}

	// ② 配了规则的供应商，链上必须有 原→新 与命中规则三要素。
	redirect, ok := item["modelRedirect"].(map[string]any)
	if !ok {
		t.Fatalf("配了重定向规则的条目缺少 modelRedirect：%v", item)
	}
	if redirect["originalModel"] != integrationModel {
		t.Fatalf("modelRedirect.originalModel = %v，期望 %q", redirect["originalModel"], integrationModel)
	}
	if redirect["redirectedModel"] != redirectTarget {
		t.Fatalf("modelRedirect.redirectedModel = %v，期望 %q", redirect["redirectedModel"], redirectTarget)
	}
	// 计费依据是用户请求的模型（Node 的 `billingModel: originalModel`）。
	if redirect["billingModel"] != integrationModel {
		t.Fatalf("modelRedirect.billingModel = %v，期望 %q", redirect["billingModel"], integrationModel)
	}
	rule, ok := redirect["matchedRule"].(map[string]any)
	if !ok {
		t.Fatalf("modelRedirect 缺少 matchedRule：%v", redirect)
	}
	if rule["matchType"] != "exact" || rule["source"] != integrationModel || rule["target"] != redirectTarget {
		t.Fatalf("matchedRule 三要素不符：%v", rule)
	}
}

// TestIntegrationStreamChainCarriesUpstreamError 钉住失败条目的状态码与错误信息。
//
// 上游恒返 500：请求最终非 2xx，链上必须留下该次尝试的真实状态码与错误文本
// （前端「错误」区块与状态码徽标就读这两个键）。
func TestIntegrationStreamChainCarriesUpstreamError(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusInternalServerError)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	status, _ := integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	if status == http.StatusOK {
		t.Fatalf("上游恒 500 时网关不应回 200，实际 %d", status)
	}

	// 失败路径会故障转移到库里其它供应商，故按「链中含夹具供应商」定位该次请求的行。
	row := latestRowContainingProvider(t, provisioned, provisioned.providerID)
	chain := providerChainOf(t, row)
	if len(chain) == 0 {
		t.Fatalf("失败请求也应留下链：%v", row["provider_chain"])
	}
	item := chainEntryFor(t, chain, provisioned.providerID)

	code, ok := item["statusCode"].(float64)
	if !ok || int(code) != http.StatusInternalServerError {
		t.Fatalf("失败条目 statusCode = %v，期望 500：%v", item["statusCode"], item)
	}
	msg, _ := item["errorMessage"].(string)
	if msg == "" {
		t.Fatalf("失败条目应有 errorMessage：%v", item)
	}
}
