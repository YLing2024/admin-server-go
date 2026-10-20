// Package httpapi 用标准库 net/http 复刻旧实现（Express 4）的 HTTP 契约。
//
// 装配顺序与旧 src/index.ts 逐条对齐：请求体解析（默认 100kb）→ system / auth / api-tokens /
// term / public-apps / history 各路由（顺序即优先级）→ 未知路径按 Express 默认 404 兜底。
//
// 业务实现放在各 internal/<domain> 包，本包只负责路由分发、鉴权包装与全局行为（含鉴权前解析请求体）。
package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"admin-server-go/internal/auth"
	"admin-server-go/internal/config"
	"admin-server-go/internal/history"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/state"
	"admin-server-go/internal/term"
)

// Handler 是路由处理函数。实现 http.Handler，便于直接交给鉴权中间件包装。
type Handler func(http.ResponseWriter, *http.Request)

// ServeHTTP 让 Handler 满足 http.Handler。
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h(w, r) }

// MetricsService 是系统监控域（internal/metrics 实现）。
type MetricsService interface {
	StartSampler()
	System(w http.ResponseWriter, r *http.Request)
	History(w http.ResponseWriter, r *http.Request)
	Metrics(w http.ResponseWriter, r *http.Request)
	Stream(w http.ResponseWriter, r *http.Request)
	Services(w http.ResponseWriter, r *http.Request)
}

// AppsService 是应用探活面板域（internal/apps 实现），只服务公开接口。
type AppsService interface {
	PublicApps(w http.ResponseWriter, r *http.Request)
}

// SystemInfoService 是「系统信息」域（internal/sysinfo 实现）：只读概况与 Top 进程。
type SystemInfoService interface {
	Info(w http.ResponseWriter, r *http.Request)
	Processes(w http.ResponseWriter, r *http.Request)
}

// SSHKeysService 是 SSH 公钥白名单管理域（internal/sshkeys 实现）。
type SSHKeysService interface {
	List(w http.ResponseWriter, r *http.Request)
	Create(w http.ResponseWriter, r *http.Request)
	Update(w http.ResponseWriter, r *http.Request)
	Delete(w http.ResponseWriter, r *http.Request)
}

// Options 是装配参数。
type Options struct {
	Cfg     *config.Config
	Files   *config.FileStore
	Redis   *state.Client
	Log     *slog.Logger
	Metrics MetricsService
	Apps    AppsService
	SSHKeys SSHKeysService
	System  SystemInfoService
}

// Server 是 HTTP 装配体。
type Server struct {
	cfg     *config.Config
	files   *config.FileStore
	redis   *state.Client
	log     *slog.Logger
	auth    *auth.Authenticator
	metrics MetricsService
	apps    AppsService
	sshKeys SSHKeysService
	system  SystemInfoService
	term    *term.Service
	history *history.Service

	// 登录失败锁定（内存按 IP）：连续 5 次失败锁 60 秒。
	loginLimiter loginLimiter
	// TOTP 模块 /login 的分档限速器。
	totpLimiter *auth.RateLimiter
	routes      []apiRoute
}

// New 构造服务并解析路由表。
func New(opts Options) *Server {
	s := &Server{
		cfg:         opts.Cfg,
		files:       opts.Files,
		redis:       opts.Redis,
		log:         opts.Log,
		metrics:     opts.Metrics,
		apps:        opts.Apps,
		sshKeys:     opts.SSHKeys,
		system:      opts.System,
		term:        term.New(term.Options{Cfg: opts.Cfg, Redis: opts.Redis, Log: opts.Log}),
		history:     history.New(history.Options{Cfg: opts.Cfg, Log: opts.Log}),
		totpLimiter: auth.NewRateLimiter(5, []int{60, 300, 900}),
	}
	s.auth = auth.New(opts.Cfg, opts.Files, opts.Redis, opts.Log)
	s.routes = s.routeTable()
	return s
}

// ServeHTTP 完成请求体解析与路由分发。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseBody(r)
	if err != nil {
		// 与旧实现一致：express.json 解析/超限错误落到统一错误处理器 → 500。
		httpx.Error(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	// HEAD 由 GET 路由承接（Express 语义）：处理器照常执行，但正文丢弃、
	// Content-Length 取 GET 会产生的正文长度。用 headWriter 统一承接所有出口。
	var hw *headWriter
	out := w
	if req.Method == http.MethodHead {
		hw = &headWriter{ResponseWriter: w}
		out = hw
	}
	s.route(out, req)
	if hw != nil {
		hw.finish()
	}
}

// route 按路由表分发；命中即执行并返回。未命中时按 Express 语义处理 OPTIONS / 默认 404。
func (s *Server) route(w http.ResponseWriter, req *http.Request) {
	// sso 模式下本地 TOTP 首次设置整体不提供：旧实现用路径级 router.use 守卫，
	// 对任意方法（含 OPTIONS/HEAD）都返回 404 JSON，而不是退回默认 404 页或自动 OPTIONS。
	if s.cfg.AuthMode == config.AuthSSO && trimTrailingSlash(req.URL.Path) == "/api/admin/totp/setup" {
		httpx.Error(w, http.StatusNotFound, "Not Found")
		return
	}
	// HEAD 匹配 GET 路由（Express 的 route._handles_method 会把 HEAD 归到 GET）。
	matched := req.Method
	if matched == http.MethodHead {
		matched = http.MethodGet
	}
	for i := range s.routes {
		rt := &s.routes[i]
		params, ok := matchRoute(rt, req, matched)
		if !ok {
			continue
		}
		if rt.builtinOnly && s.cfg.AuthMode == config.AuthSSO {
			httpx.Error(w, http.StatusNotFound, "Not Found")
			return
		}
		routed := httpx.WithParams(req, params)
		switch rt.auth {
		case authRequired:
			s.auth.Require(rt.handler).ServeHTTP(w, routed)
		default:
			rt.handler.ServeHTTP(w, routed)
		}
		return
	}
	// 未匹配任何方法：OPTIONS 走 Express 的自动 Allow 响应（免鉴权），否则默认 404。
	if req.Method == http.MethodOptions {
		if methods := s.allowedMethods(req); len(methods) > 0 {
			body := strings.Join(methods, ",")
			w.Header().Set("Allow", body)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	s.notFound(w, req)
}

// allowedMethods 复刻 Express 对未匹配 OPTIONS 的自动响应：按注册顺序收集所有「路径命中」
// 路由的方法并去重，GET 之后追加 HEAD；`.all`/`use` 中间件不参与（由处理器自身响应）。
// 不涉及鉴权，命中路由的鉴权要求此处一律忽略——与旧实现一致。
func (s *Server) allowedMethods(r *http.Request) []string {
	methods := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(m string) {
		if !seen[m] {
			seen[m] = true
			methods = append(methods, m)
		}
	}
	for i := range s.routes {
		rt := &s.routes[i]
		if _, ok := matchPath(rt, r.URL.Path); !ok {
			continue
		}
		if rt.method == "ALL" {
			continue
		}
		add(rt.method)
		if rt.method == http.MethodGet {
			add(http.MethodHead)
		}
	}
	return methods
}

// headWriter 承接 HEAD 请求：处理器照常写响应，这里只统计正文字节数并丢弃正文，
// finish 时补上 Content-Length（即 GET 会产生的正文长度），与 Express HEAD 语义一致。
type headWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bodyLen     int
}

func (h *headWriter) WriteHeader(code int) {
	if h.wroteHeader {
		return
	}
	h.status = code
	h.wroteHeader = true
}

func (h *headWriter) Write(b []byte) (int, error) {
	h.WriteHeader(http.StatusOK)
	h.bodyLen += len(b)
	return len(b), nil
}

// finish 落定状态码并补 Content-Length；204/304 无正文，不补。
func (h *headWriter) finish() {
	h.WriteHeader(http.StatusOK)
	if h.status != http.StatusNoContent && h.status != http.StatusNotModified {
		if h.ResponseWriter.Header().Get("Content-Length") == "" {
			h.ResponseWriter.Header().Set("Content-Length", strconv.Itoa(h.bodyLen))
		}
	}
	h.ResponseWriter.WriteHeader(h.status)
}

// parseBody 在鉴权之前解析 JSON / 表单请求体（对齐 app.use(express.json) 的执行时机）。
// multipart 与其它类型原样放行，由各 handler 自行读取。
func (s *Server) parseBody(r *http.Request) (*http.Request, error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	isJSON := strings.HasPrefix(ct, "application/json")
	isForm := strings.HasPrefix(ct, "application/x-www-form-urlencoded")
	if !isJSON && !isForm {
		return r, nil
	}
	limit := int64(100 * 1024)
	data, err := readLimited(r.Body, limit)
	if err != nil {
		return nil, err
	}
	obj, err := httpx.ParseBody(ct, limit, data)
	if err != nil {
		return nil, err
	}
	return httpx.WithBody(r, obj), nil
}

// readLimited 最多读取 limit 字节；超出返回错误（对齐 body-parser 的 limit 语义）。
func readLimited(body io.Reader, limit int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

type bodyTooLarge struct{}

func (bodyTooLarge) Error() string { return "请求体过大" }

var errBodyTooLarge = bodyTooLarge{}

// notFound 复刻 Express finalhandler 的默认 404 页面（未知路径 / 未匹配方法）。
// HEAD 的正文丢弃由调用方的 headWriter 负责，这里一律照写以便统计 Content-Length。
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	msg := "Cannot " + r.Method + " " + r.URL.Path
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>"+escapeHTML(msg)+"</pre>\n</body>\n</html>\n")
}

func escapeHTML(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(s)
}
