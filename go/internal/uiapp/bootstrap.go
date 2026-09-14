package uiapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// Meta 是壳注入里的站点元数据。
//
// 空字段一律省略（omitempty）：前端对这些字段都有自带兜底（站点标题见 `DEFAULT_SITE_TITLE`），
// 因此「取不到」与「取到空串」都不该编出一个假值——省略字段才是如实表达。
type Meta struct {
	SiteTitle string
	TimeZone  string
	Version   string
}

// MetaSource 按请求读取站点元数据（典型实现读 system_settings）。
//
// 它被 New 以 defaultMetaTTL 缓存：壳是热路径，而站点标题/时区只在设置页改动。
type MetaSource func(ctx context.Context) (Meta, error)

// Session 是壳注入里的会话快照。
//
// 字段名与前端契约逐字对齐（src/components/ui-session-gate.tsx 的 UiSessionSnapshot）：
// `user.role` 是角色敏感重定向的唯一依据（回退探针拿不到它，见该文件的说明）。
type Session struct {
	User UserSnapshot `json:"user"`
	// Key 为 nil 时不输出：Node 的会话快照里 key 只在有密钥上下文时存在。
	Key *KeySnapshot `json:"key,omitempty"`
}

// UserSnapshot 是会话里的用户。
type UserSnapshot struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
	// Role 取 "admin" / "user"；未知时省略（前端按「角色未知」处理，不猜）。
	Role string `json:"role,omitempty"`
}

// KeySnapshot 是会话所用密钥的 Web UI 登录权限（Node 的 session.key.canLoginWebUi）。
type KeySnapshot struct {
	CanLoginWebUI bool `json:"canLoginWebUi"`
}

// SessionResolver 解析当前请求的会话；未登录时返回 ok=false 且 err=nil。
//
// 为什么是接口而不是本包自己读 Redis/库：会话解析（不透明会话、裸 Key、签名令牌三态）的
// 唯一实现在 internal/adminapi 的 AuthGuard。本包只消费「已解析的会话快照」，
// 解析口径一旦出现第二份就是漂移。err 只在依赖故障时非 nil，此时按未登录处理并记 warn。
type SessionResolver interface {
	ResolveSession(ctx context.Context, r *http.Request) (Session, bool, error)
}

// SessionResolverFunc 把函数适配成 SessionResolver。
type SessionResolverFunc func(ctx context.Context, r *http.Request) (Session, bool, error)

// ResolveSession 实现 SessionResolver。
func (f SessionResolverFunc) ResolveSession(ctx context.Context, r *http.Request) (Session, bool, error) {
	return f(ctx, r)
}

// bootstrap 是注入进壳的引导数据。
//
// 契约与前端逐字对齐（ui-session-gate.tsx 的 UiBootstrap）；`session` 的三态是核心语义：
// 字段缺失 = 未注入（前端走回退探针）、null = 已注入且未登录、对象 = 已登录。
// 本包**恒注入**，故只会出现后两种——回退探针那条路径在 embed 终态里不再被走到。
type bootstrap struct {
	Locale    string   `json:"locale"`
	SiteTitle string   `json:"siteTitle,omitempty"`
	TimeZone  string   `json:"timeZone,omitempty"`
	Version   string   `json:"version,omitempty"`
	Session   *Session `json:"session"`
}

// bootstrapTag 是注入的整段脚本。
//
// 安全性：json.Marshal 默认转义 `<`、`>`、`&`（为 \u003c 等），因此站点标题里出现
// `</script>` 也无法闭合脚本标签；这层转义是注入安全的前提，不得换成不转义的编码器。
func bootstrapTag(payload bootstrap) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	tag := make([]byte, 0, len(encoded)+40)
	tag = append(tag, "<script>window.__CCH_BOOTSTRAP__="...)
	tag = append(tag, encoded...)
	tag = append(tag, "</script>"...)
	return tag, nil
}

// inject 把引导数据插进壳 HTML；返回注入后的正文与是否注入成功。
//
// 落点优先级：壳里的显式标记（bootstrapMarker，被整段替换）→ `</head>` 之前 →
// 两者都不存在则不注入（改一个结构异常的壳比不注入更危险，且壳是我们自己的产物）。
// 一切失败都**不阻断响应**：壳照原样返回，前端落到回退探针，只是多一次请求。
func (h *Handler) inject(
	ctx context.Context,
	r *http.Request,
	body []byte,
	locale string,
) ([]byte, bool) {
	if len(body) > h.shellMax {
		h.logger.Warn("uiapp_shell_too_large_to_inject", map[string]any{
			"path":     r.URL.Path,
			"bytes":    len(body),
			"maxBytes": h.shellMax,
		})
		return body, false
	}

	meta := h.shellMeta(ctx)
	tag, err := bootstrapTag(bootstrap{
		Locale:    locale,
		SiteTitle: meta.SiteTitle,
		TimeZone:  meta.TimeZone,
		Version:   meta.Version,
		Session:   h.sessionOf(ctx, r),
	})
	if err != nil {
		// 只有不可编码的内容才会走到这里（当前结构下不会发生），仍不阻断响应。
		h.logger.Warn("uiapp_bootstrap_encode_failed", map[string]any{
			"path":  r.URL.Path,
			"error": err.Error(),
		})
		return body, false
	}

	if idx := bytes.Index(body, []byte(bootstrapMarker)); idx >= 0 {
		// 标记被整段替换：产物里留着标记会让第二次注入（或人工插入）变成两段引导数据。
		out := make([]byte, 0, len(body)+len(tag))
		out = append(out, body[:idx]...)
		out = append(out, tag...)
		out = append(out, body[idx+len(bootstrapMarker):]...)
		return out, true
	}
	if idx := bytes.Index(body, []byte(headClose)); idx >= 0 {
		out := make([]byte, 0, len(body)+len(tag))
		out = append(out, body[:idx]...)
		out = append(out, tag...)
		out = append(out, body[idx:]...)
		return out, true
	}
	h.logger.Warn("uiapp_shell_injection_point_missing", map[string]any{
		"path": r.URL.Path,
		"hint": "壳里既没有 " + bootstrapMarker + " 也没有 " + headClose + "，引导数据未注入",
	})
	return body, false
}

// sessionOf 解析会话；任何失败都退成「未登录」（登录态判定宁可少给角色，也不猜）。
func (h *Handler) sessionOf(ctx context.Context, r *http.Request) *Session {
	if h.sessions == nil {
		return nil
	}
	session, ok, err := h.sessions.ResolveSession(ctx, r)
	if err != nil {
		h.logger.Warn("uiapp_session_resolve_failed", map[string]any{
			"path":  r.URL.Path,
			"error": err.Error(),
		})
		return nil
	}
	if !ok {
		return nil
	}
	return &session
}

// shellMeta 取壳元数据，按 metaTTL 缓存在进程内。
//
// 取失败时沿用上一次的值并照样延长窗口：否则依赖故障期间每次页面请求都去打一次库，
// 把「单页慢」放大成「全站慢」，而这里的数据本来就不是实时要求。
func (h *Handler) shellMeta(ctx context.Context) Meta {
	if h.meta == nil {
		return Meta{}
	}
	now := h.now()

	h.mu.Lock()
	cached, until := h.cachedMeta, h.metaUntil
	h.mu.Unlock()
	if now.Before(until) {
		return cached
	}

	meta, err := h.meta(ctx)
	if err != nil {
		h.logger.Warn("uiapp_meta_unavailable", map[string]any{"error": err.Error()})
		meta = cached
	}
	h.mu.Lock()
	h.cachedMeta, h.metaUntil = meta, now.Add(h.metaTTL)
	h.mu.Unlock()
	return meta
}
