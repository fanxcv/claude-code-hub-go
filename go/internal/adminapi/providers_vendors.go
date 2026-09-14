package adminapi

import (
	"net/http"
	"sort"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
)

// 本文件实现 **供应商→厂商重挂**端点：
//
//	POST /providers/vendors:recluster   router.ts:888 → handlers.ts:537 reclusterProviderVendors
//
// 唯一真源：src/actions/providers.ts:5748-5911（reclusterProviderVendors）。
//
// 语义要点（照抄 Node 会错的地方都标了出处）：
//
//  1. 扫描面是 **全部未软删供应商**（findAllProvidersFresh，含禁用），不是「可见供应商」——
//     这条是维护工具，不做可见性过滤，也不受 hidden provider type 影响。
//  2. 比较的是「厂商域名」而不是 vendor id：`newVendorDomain = computeVendorKey(...)`，
//     `oldVendorDomain = 当前 vendor 行的 website_domain ?? ""`；两者**字符串不等**才算一条 change。
//     注意 computeVendorKey 可能返回 null（URL 解析不出域名）→ 计入 `skippedInvalidUrl` 并跳过。
//  3. preview 与 apply 返回**同一个** preview 结构；apply 只是多把 applied 置 true。
//     `vendorsCreated` 是**新域名去重计数**，`vendorsToDelete` 是**旧 vendor id 去重计数**
//     （不是 change 条数）。
//  4. apply 的四步顺序与 Node 一致：重挂（单事务）→ 端点回填 → 逐个删空厂商（失败仅告警）
//     → 广播 providers 域失效（失败仅告警）。后两步是 best-effort，不得让整个操作失败。
//  5. `{confirm}` 省略即 false（Node 的 `z.boolean().default(false)`），且对象是 strict。

// providerReclusterChange 逐字对应 ReclusterChange（actions/providers.ts:5723-5729）。
type providerReclusterChange struct {
	ProviderID      int64  `json:"providerId"`
	ProviderName    string `json:"providerName"`
	OldVendorID     int64  `json:"oldVendorId"`
	OldVendorDomain string `json:"oldVendorDomain"`
	NewVendorDomain string `json:"newVendorDomain"`
}

// providerReclusterPreview 逐字对应 ReclusterResult.preview（actions/providers.ts:5732-5737）。
type providerReclusterPreview struct {
	ProvidersMoved    int `json:"providersMoved"`
	VendorsCreated    int `json:"vendorsCreated"`
	VendorsToDelete   int `json:"vendorsToDelete"`
	SkippedInvalidURL int `json:"skippedInvalidUrl"`
}

// providerReclusterResponse 逐字对应 ReclusterResult。
type providerReclusterResponse struct {
	Preview providerReclusterPreview  `json:"preview"`
	Changes []providerReclusterChange `json:"changes"`
	Applied bool                      `json:"applied"`
}

// RegisterProvidersVendorsRoutes 注册厂商重挂端点。
//
// Store 未装配即不注册（回退 Node）：这条要读全量供应商行、厂商域名并做写事务，
// 少了库就只能答一张假表。
func RegisterProvidersVendorsRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_vendors_unwired", map[string]any{
				"module": "providers",
				"action": "recluster_route_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers/vendors:recluster",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "reclusterProviderVendors",
		Handler:     http.HandlerFunc(handleReclusterProviderVendors(deps)),
	})
}

// handleReclusterProviderVendors 复刻 reclusterProviderVendors。
func handleReclusterProviderVendors(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "confirm")
		object.RejectUnknownKeys()
		confirm, _ := object.Bool("confirm")
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		confirmValue := confirm != nil && *confirm

		ctx := request.Context()
		providers, err := deps.Store.AdminListProvidersForRecluster(ctx)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if len(providers) == 0 {
			// Node：空表直接返回零值 preview（不查厂商、不触碰任何东西）。
			adminWriteJSON(writer, http.StatusOK, providerReclusterResponse{
				Preview: providerReclusterPreview{},
				Changes: []providerReclusterChange{},
				Applied: confirmValue,
			})
			return
		}

		// 当前厂商域名：一次性批量读（Node 的 findProviderVendorsByIds，避免 N+1）。
		vendorIDs := make([]int64, 0, len(providers))
		seenVendor := make(map[int64]struct{}, len(providers))
		for _, provider := range providers {
			if provider.ProviderVendorID == nil || *provider.ProviderVendorID <= 0 {
				continue
			}
			if _, duplicate := seenVendor[*provider.ProviderVendorID]; duplicate {
				continue
			}
			seenVendor[*provider.ProviderVendorID] = struct{}{}
			vendorIDs = append(vendorIDs, *provider.ProviderVendorID)
		}
		domains, err := deps.Store.AdminProviderVendorDomainsByIDs(ctx, vendorIDs)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		changes := make([]providerReclusterChange, 0, len(providers))
		assignments := make(map[int64]string, len(providers))
		newVendorKeys := make(map[string]struct{})
		oldVendorIDs := make(map[int64]struct{})
		skippedInvalidURL := 0
		currentDomains := make(map[int64]string, len(providers))

		for _, provider := range providers {
			newVendorKey, keyErr := providerVendorDomainKey(map[string]any{
				"url":         provider.URL,
				"website_url": provider.WebsiteURL,
			})
			if keyErr != nil || newVendorKey == "" {
				// Node 的 computeVendorKey 返回 null 分支（URL 解析不出域名）。
				skippedInvalidURL++
				continue
			}
			currentDomain := ""
			if provider.ProviderVendorID != nil {
				currentDomain = domains[*provider.ProviderVendorID]
			}
			currentDomains[provider.ID] = currentDomain
			if currentDomain == newVendorKey {
				continue
			}
			var oldVendorID int64
			if provider.ProviderVendorID != nil {
				oldVendorID = *provider.ProviderVendorID
				if oldVendorID > 0 {
					oldVendorIDs[oldVendorID] = struct{}{}
				}
			}
			newVendorKeys[newVendorKey] = struct{}{}
			assignments[provider.ID] = newVendorKey
			changes = append(changes, providerReclusterChange{
				ProviderID:      provider.ID,
				ProviderName:    provider.Name,
				OldVendorID:     oldVendorID,
				OldVendorDomain: currentDomain,
				NewVendorDomain: newVendorKey,
			})
		}

		preview := providerReclusterPreview{
			ProvidersMoved:    len(changes),
			VendorsCreated:    len(newVendorKeys),
			VendorsToDelete:   len(oldVendorIDs),
			SkippedInvalidURL: skippedInvalidURL,
		}
		if !confirmValue || len(changes) == 0 {
			// Node 在预览分支与「confirm 属实但无变更」分支都走这一条返回：
			// applied 就等于 confirm（confirm=true 且无变更时 Node 也回 applied: true）。
			adminWriteJSON(writer, http.StatusOK, providerReclusterResponse{
				Preview: preview,
				Changes: changes,
				Applied: confirmValue,
			})
			return
		}

		// ① 重挂（单事务）：Node 在 db.transaction 里逐条 getOrCreate + update。
		if _, err := deps.Store.AdminReassignProviderVendors(ctx, assignments); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		// ② 端点回填（Node 的 backfillProviderEndpointsFromProviders）。
		if _, err := deps.Store.AdminBackfillProviderEndpointsFromProviders(ctx); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		// ③ 清理空厂商：逐个尝试，失败仅告警（Node 的 try/catch + logger.warn）。
		for _, vendorID := range sortedVendorIDs(oldVendorIDs) {
			deleted, err := deps.Store.AdminDeleteProviderVendorIfEmpty(ctx, vendorID)
			if err != nil {
				adminLoggerOf(deps).Warn("admin_providers_vendors_cleanup_failed", map[string]any{
					"vendorId": vendorID,
					"error":    err.Error(),
				})
				continue
			}
			if deleted {
				adminLoggerOf(deps).Debug("admin_providers_vendors_cleanup_deleted", map[string]any{
					"vendorId": vendorID,
				})
			}
		}
		// ④ 广播 providers 域失效（失败仅告警）。
		adminPublishDomain(deps, request, cfgsync.DomainProviders)

		adminWriteJSON(writer, http.StatusOK, providerReclusterResponse{
			Preview: preview,
			Changes: changes,
			Applied: true,
		})
	}
}

// sortedVendorIDs 把集合按键升序展开（清理顺序确定，便于复现与对账）。
func sortedVendorIDs(ids map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
