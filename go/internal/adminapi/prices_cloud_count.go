package adminapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是 GET /api/prices/cloud-model-count 的 Go 落点
// （Node 侧 src/app/api/prices/cloud-model-count/route.ts）。
//
// 形状：成功 `{"ok":true,"data":{"count":N,"version":"..."}}`；抓取失败 **502** +
// `{"ok":false,"error":"<原因>"}`。它是页面自己 fetch 的私有端点（不是 /api/v1 资源面），
// 故走 NoManagementEnvelope 的裸 JSON。
//
// **一处刻意的偏离：加缓存**。Node 是「每请求抓一次」——那条路径每次要拉 28 MiB 正文再解析
// （实测云端表约 1.1 万模型），一次管理页点击就是一次全量下载，连续点几次就打几次。
// 这里的口径是：**成功结果缓存 60 秒**，并做 single-flight 合并（同刻的并发请求只抓一次）。
// TTL 取 60 秒的依据：
//   - 消费方是价格同步状态卡片，它问的是「云端现在有多少模型、版本是什么」，不是逐请求的实时值；
//   - 60 秒内点击两次会得到**同一个版本号**，比 Node 的「两次请求可能给出不同结果」更可解释；
//   - 与仓库里其它「云端目录类」读的缓存量级一致（system settings 的 60 秒 TTL）。
//
// 失败**不进缓存**：一次网络抖动不该让这个端点连续一分钟都错。

// cloudModelCountCacheTTL 是成功结果的缓存时长（见文件头依据）。
const cloudModelCountCacheTTL = 60 * time.Second

// CloudPriceTableSource 抓一次云端 CPT 价格表（实现见 internal/jobs 的 CloudPriceTableSource）。
//
// nil 表示未装配：这条路由不注册，原样回退 Node。
type CloudPriceTableSource interface {
	FetchCloudPriceTable(ctx context.Context) (*jobs.CptTable, error)
}

// RegisterCloudModelCountRoutes 注册云端模型数端点（源未装配时不注册）。
func RegisterCloudModelCountRoutes(router *Router, deps Deps) {
	if deps.CloudPriceTables == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_cloud_price_count_unwired", map[string]any{
				"module": "model_price",
				"action": "route_not_registered",
			})
		}
		return
	}
	api := &cloudModelCountAPI{
		source: deps.CloudPriceTables,
		logger: adminLoggerOf(deps),
	}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/prices/cloud-model-count",
		Access:               AccessAdmin,
		Module:               "model_price",
		OperationID:          "getCloudModelCount",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handle),
	})
}

type cloudModelCountAPI struct {
	source CloudPriceTableSource
	logger *logx.Logger

	mu       sync.Mutex
	cached   *cloudModelCount
	inflight chan struct{}
}

// cloudModelCount 是一条缓存项（成功结果）。
type cloudModelCount struct {
	count     int
	version   string
	expiresAt time.Time
}

// handle 复刻 route.ts 的 GET：抓取 → 计数 → 作答。
func (api *cloudModelCountAPI) handle(writer http.ResponseWriter, request *http.Request) {
	count, version, err := api.lookup(request.Context())
	if err != nil {
		api.logger.Warn("admin_cloud_model_count_fetch_failed", map[string]any{"error": err.Error()})
		adminWriteJSON(writer, http.StatusBadGateway,
			map[string]any{"ok": false, "error": err.Error()})
		return
	}
	adminWriteJSON(writer, http.StatusOK, map[string]any{
		"ok": true,
		"data": map[string]any{
			"count":   count,
			"version": version,
		},
	})
}

// lookup 走缓存 + single-flight：同刻并发只抓一次，成功后缓存 60 秒。
func (api *cloudModelCountAPI) lookup(ctx context.Context) (int, string, error) {
	for {
		api.mu.Lock()
		if api.cached != nil && time.Now().Before(api.cached.expiresAt) {
			entry := *api.cached
			api.mu.Unlock()
			return entry.count, entry.version, nil
		}
		if api.inflight == nil {
			// 本路成为抓取者：先立 inflight，再在锁外抓（持锁抓 30 秒会把其它请求也钉住）。
			api.inflight = make(chan struct{})
			done := api.inflight
			api.mu.Unlock()

			count, version, err := api.fetch(ctx)

			api.mu.Lock()
			if err == nil {
				api.cached = &cloudModelCount{
					count:     count,
					version:   version,
					expiresAt: time.Now().Add(cloudModelCountCacheTTL),
				}
			}
			api.inflight = nil
			api.mu.Unlock()
			close(done)
			return count, version, err
		}
		// 已有一路在抓：等它结束再复用结果（避免一次并发点击打出 N 次 28 MiB 下载）。
		done := api.inflight
		api.mu.Unlock()
		select {
		case <-done:
			// 回到循环顶部重判：前一路成功则命中缓存，失败则本路自己抓。
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
}

// fetch 抓一次并解析（不持锁）。
func (api *cloudModelCountAPI) fetch(ctx context.Context) (int, string, error) {
	table, err := api.source.FetchCloudPriceTable(ctx)
	if err != nil {
		return 0, "", err
	}
	return len(table.Models), table.Version, nil
}
