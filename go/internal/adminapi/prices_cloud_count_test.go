package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// jobsCptTable 是 jobs.CptTable 的别名（用例里只关心 Models 与 Version 两个字段）。
type jobsCptTable = jobs.CptTable

// newJobsCptTable 造一张最小 CPT 表（模型数用占位条目凑）。
func newJobsCptTable(models int, version string) *jobs.CptTable {
	table := &jobs.CptTable{Version: version}
	for index := 0; index < models; index++ {
		table.Models = append(table.Models, jobs.CptModelEntry{})
	}
	return table
}

// 本文件是 /api/prices/cloud-model-count 的用例（不依赖真库：这条端点只读外网价格表）。
//
// 三个要点：形状（ok 包裹 / 502 错误体）、缓存命中（第二次不再抓）、single-flight（并发只抓一次）。

// fakeCloudPriceSource 是 CloudPriceTableSource 的替身：计数调用次数，可注入错误与延迟。
type fakeCloudPriceSource struct {
	calls   atomic.Int64
	models  int
	version string
	err     error
	delay   time.Duration
}

func (f *fakeCloudPriceSource) FetchCloudPriceTable(context.Context) (*jobsCptTable, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return nil, f.err
	}
	return newJobsCptTable(f.models, f.version), nil
}

func cloudCountRouter(source CloudPriceTableSource) *Router {
	deps := Deps{
		Logger:           logx.New(nil),
		Guard:            principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		CloudPriceTables: source,
	}
	router := New(Options{Deps: deps})
	RegisterCloudModelCountRoutes(router, deps)
	return router
}

func TestCloudModelCountShapeAndCache(t *testing.T) {
	source := &fakeCloudPriceSource{models: 4, version: "v-2026-09-12"}
	router := cloudCountRouter(source)

	status, body := call(router, http.MethodGet, "/api/prices/cloud-model-count", "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d：%s", status, body)
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("正文不是 JSON：%s", body)
	}
	if payload["ok"] != true {
		t.Fatalf("应带 ok:true：%s", body)
	}
	data, _ := payload["data"].(map[string]any)
	if data["count"].(float64) != 4 || data["version"] != "v-2026-09-12" {
		t.Fatalf("data 形状不符：%s", body)
	}

	// 第二次请求命中 60 秒缓存：抓取次数仍为 1。
	call(router, http.MethodGet, "/api/prices/cloud-model-count", "")
	if got := source.calls.Load(); got != 1 {
		t.Fatalf("TLL 内应只抓一次，实得 %d 次", got)
	}
}

func TestCloudModelCountFetchFailureIsBadGateway(t *testing.T) {
	source := &fakeCloudPriceSource{err: errors.New("云端价格表拉取失败：HTTP 503")}
	router := cloudCountRouter(source)

	status, body := call(router, http.MethodGet, "/api/prices/cloud-model-count", "")
	if status != http.StatusBadGateway {
		t.Fatalf("抓取失败应 502，实得 %d：%s", status, body)
	}
	if !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "HTTP 503") {
		t.Fatalf("错误体应带 ok:false 与原因：%s", body)
	}

	// 失败不进缓存：下一次请求必须再抓一次。
	call(router, http.MethodGet, "/api/prices/cloud-model-count", "")
	if got := source.calls.Load(); got != 2 {
		t.Fatalf("失败不应缓存，实得 %d 次抓取", got)
	}
}

func TestCloudModelCountSingleFlight(t *testing.T) {
	source := &fakeCloudPriceSource{models: 2, version: "v1", delay: 50 * time.Millisecond}
	router := cloudCountRouter(source)

	const parallel = 8
	var wait sync.WaitGroup
	wait.Add(parallel)
	for index := 0; index < parallel; index++ {
		go func() {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodGet, "/api/prices/cloud-model-count", nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Errorf("并发请求应全部 200，实得 %d", recorder.Code)
			}
		}()
	}
	wait.Wait()

	if got := source.calls.Load(); got != 1 {
		t.Fatalf("同刻并发应合并成一次抓取，实得 %d 次", got)
	}
}

func TestCloudModelCountRouteNotRegisteredWithoutSource(t *testing.T) {
	deps := Deps{
		Logger: logx.New(nil),
		Guard:  principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
	}
	router := New(Options{Deps: deps})
	RegisterCloudModelCountRoutes(router, deps)

	status, _ := call(router, http.MethodGet, "/api/prices/cloud-model-count", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配数据源时应未注册（落到回退位），实得 %d", status)
	}
}
