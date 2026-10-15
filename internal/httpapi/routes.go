package httpapi

import (
	"net/http"
	"strings"
)

// routeAuth 是路由的鉴权要求。
type routeAuth int

const (
	// authNone 免鉴权。
	authNone routeAuth = iota
	// authRequired 业务鉴权（builtin 会话 / sso 头）。
	authRequired
	// authNotificationsWrite 通知写入通道（回环直连 / 可写令牌 / 回落业务鉴权）。
	authNotificationsWrite
)

// apiRoute 是一条路由。pattern 既是匹配依据，也是 scripts/check-api-docs.mjs 与 docs/API.md
// 机检对照的字面量锚点（形如 "METHOD /path"）。顺序即优先级。
type apiRoute struct {
	pattern     string
	method      string
	routePath   string
	auth        routeAuth
	builtinOnly bool
	handler     Handler
}

// routeTable 是全部 API 路由，按旧 src/index.ts 的挂载顺序排列。
func (s *Server) routeTable() []apiRoute {
	routes := []apiRoute{
		// ① system（最早挂载）：根信息 / 系统信息 / 历史与聚合 / SSE / 服务状态
		{pattern: "GET /", handler: s.handleRoot},
		{pattern: "GET /api/admin/system", auth: authRequired, handler: s.metrics.System},
		{pattern: "GET /api/admin/system/history", auth: authRequired, handler: s.metrics.History},
		{pattern: "GET /api/admin/system/metrics", auth: authRequired, handler: s.metrics.Metrics},
		{pattern: "GET /api/admin/system/stream", auth: authRequired, handler: s.metrics.Stream},
		{pattern: "GET /api/admin/services", auth: authRequired, handler: s.metrics.Services},

		// ② auth：模式探测 / 本地登录登出 / 当前身份 / TOTP / 设备会话转发 / 改密
		{pattern: "GET /api/admin/auth-mode", handler: s.handleAuthMode},
		{pattern: "POST /api/admin/login", builtinOnly: true, handler: s.handleLogin},
		{pattern: "POST /api/admin/logout", builtinOnly: true, handler: s.handleLogout},
		{pattern: "GET /api/admin/me", auth: authRequired, builtinOnly: true, handler: s.handleMe},
		{pattern: "POST /api/admin/totp/reset", auth: authRequired, handler: s.forwardTotp("reset")},
		{pattern: "POST /api/admin/totp/confirm", auth: authRequired, handler: s.forwardTotp("confirm")},
		{pattern: "ALL /api/admin/sessions*", auth: authRequired, handler: s.forwardSessions},
		{pattern: "POST /api/admin/totp/setup", builtinOnly: true, handler: s.handleTotpSetup},
		{pattern: "POST /api/admin/totp/login", handler: s.handleTotpLogin},
		{pattern: "POST /api/admin/password", auth: authRequired, handler: s.handlePassword},

		// ③ api-tokens
		{pattern: "GET /api/admin/api-tokens", auth: authRequired, handler: s.handleAPITokensList},
		{pattern: "POST /api/admin/api-tokens", auth: authRequired, handler: s.handleAPITokensCreate},
		{pattern: "PATCH /api/admin/api-tokens/:id", auth: authRequired, handler: s.handleAPITokensUpdate},
		{pattern: "DELETE /api/admin/api-tokens/:id", auth: authRequired, handler: s.handleAPITokensDelete},

		// ④ notifications（固定路径先于 /:id）
		{pattern: "POST /api/admin/notifications", auth: authNotificationsWrite, handler: s.notifs.Create},
		{pattern: "GET /api/admin/notifications", auth: authRequired, handler: s.notifs.List},
		{pattern: "GET /api/admin/notifications/types", auth: authRequired, handler: s.notifs.TypesList},
		{pattern: "PATCH /api/admin/notifications/types/:key", auth: authRequired, handler: s.notifs.TypesPatch},
		{pattern: "POST /api/admin/notifications/:id/read", auth: authRequired, handler: s.notifs.Read},
		{pattern: "POST /api/admin/notifications/read-all", auth: authRequired, handler: s.notifs.ReadAll},
		{pattern: "DELETE /api/admin/notifications/:id", auth: authRequired, handler: s.notifs.Delete},
		{pattern: "POST /api/admin/notifications/bulk-delete", auth: authRequired, handler: s.notifs.BulkDelete},
		{pattern: "GET /api/admin/notifications/stats", auth: authRequired, handler: s.notifs.Stats},
		{pattern: "GET /api/admin/notifications/stream", auth: authRequired, handler: s.notifs.Stream},
		{pattern: "GET /api/admin/notifications/:id", auth: authRequired, handler: s.notifs.Get},
		{pattern: "GET /api/admin/notifications/:id/files/:name", auth: authRequired, handler: s.notifs.Attachment},

		// ⑤ versions
		{pattern: "GET /api/admin/versions", auth: authRequired, handler: s.handleVersions},

		// ⑥ term
		{pattern: "POST /api/admin/term/unlock", auth: authRequired, handler: s.term.Unlock},
		{pattern: "GET /api/admin/term/verify", handler: s.term.Verify},
		{pattern: "GET /api/admin/term/sessions", auth: authRequired, handler: s.term.ListSessions},
		{pattern: "POST /api/admin/term/sessions/close", auth: authRequired, handler: s.term.CloseSessions},
		{pattern: "DELETE /api/admin/term/sessions/:name", auth: authRequired, handler: s.term.KillSession},

		// ⑦ apps / ⑧ public-apps
		{pattern: "GET /api/admin/apps", auth: authRequired, handler: s.apps.AdminApps},
		{pattern: "GET /api/public/apps", handler: s.apps.PublicApps},

		// ⑨ files
		{pattern: "POST /api/admin/upload", auth: authRequired, handler: s.fileSvc.Upload},
		{pattern: "POST /api/admin/files/upload", auth: authRequired, handler: s.fileSvc.FilesUpload},
		{pattern: "GET /api/admin/files", auth: authRequired, handler: s.fileSvc.FilesList},
		{pattern: "GET /api/admin/files/download", auth: authRequired, handler: s.fileSvc.FilesDownload},
		{pattern: "POST /api/admin/files/mkdir", auth: authRequired, handler: s.fileSvc.FilesMkdir},
		{pattern: "POST /api/admin/files/rename", auth: authRequired, handler: s.fileSvc.FilesRename},
		{pattern: "DELETE /api/admin/files", auth: authRequired, handler: s.fileSvc.FilesDelete},
		{pattern: "GET /api/admin/download", auth: authRequired, handler: s.fileSvc.Download},

		// ⑩ shares
		{pattern: "POST /api/admin/files/shares", auth: authRequired, handler: s.shares.Create},
		{pattern: "GET /api/admin/files/shares", auth: authRequired, handler: s.shares.List},
		{pattern: "PATCH /api/admin/files/shares/:id", auth: authRequired, handler: s.shares.Update},
		{pattern: "DELETE /api/admin/files/shares/:id", auth: authRequired, handler: s.shares.Delete},
		{pattern: "GET /s/:token", handler: s.shares.PublicDownload},

		// ⑪ history（最后，且在 errorHandler 之后）
		{pattern: "GET /api/admin/history", auth: authRequired, handler: s.history.List},
		{pattern: "GET /api/admin/history/:id", auth: authRequired, handler: s.history.Detail},
	}
	for i := range routes {
		routes[i].method, routes[i].routePath = splitPattern(routes[i].pattern)
	}
	return routes
}

func splitPattern(pattern string) (string, string) {
	if idx := strings.IndexByte(pattern, ' '); idx >= 0 {
		return pattern[:idx], pattern[idx+1:]
	}
	return "", pattern
}

// matchRoute 判断请求是否命中路由，并抽取路径参数。支持 `:name` 单段占位与尾部 `*` 前缀匹配。
func matchRoute(rt *apiRoute, r *http.Request) (map[string]string, bool) {
	if rt.method != "ALL" && rt.method != r.Method {
		return nil, false
	}
	pattern := rt.routePath
	actual := trimTrailingSlash(r.URL.Path)
	if strings.HasSuffix(pattern, "*") {
		prefix := trimTrailingSlash(strings.TrimSuffix(pattern, "*"))
		if actual == prefix || strings.HasPrefix(actual, prefix+"/") {
			return map[string]string{}, true
		}
		return nil, false
	}
	pat := trimTrailingSlash(pattern)
	pseg := splitSegments(pat)
	aseg := splitSegments(actual)
	if len(pseg) != len(aseg) {
		return nil, false
	}
	params := map[string]string{}
	for i := range pseg {
		ps := pseg[i]
		if strings.HasPrefix(ps, ":") {
			params[ps[1:]] = aseg[i]
			continue
		}
		if ps != aseg[i] {
			return nil, false
		}
	}
	return params, true
}

func trimTrailingSlash(path string) string {
	if path == "" {
		return "/"
	}
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func splitSegments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
