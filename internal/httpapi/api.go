// Package httpapi 用标准库 net/http 复刻旧实现（Express 4）的 HTTP 契约。
//
// 装配顺序与旧 src/index.ts 逐条对齐：请求体解析（默认 100kb，仅 POST /api/admin/notifications
// 放宽到 24mb）→ system / auth / api-tokens / notifications / versions / term / apps / public-apps /
// files / shares / history 各路由（顺序即优先级）→ 未知路径按 Express 默认 404 兜底。
//
// 业务实现放在各 internal/<domain> 包，本包只负责路由分发、鉴权包装与全局行为（含鉴权前解析请求体）。
package httpapi

import (
	"io"
	"log/slog"
	"net/http"
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

// AppsService 是应用探活面板域（internal/apps 实现）。
type AppsService interface {
	AdminApps(w http.ResponseWriter, r *http.Request)
	PublicApps(w http.ResponseWriter, r *http.Request)
}

// NotificationsService 是通知中心域（internal/notifications 实现）。
type NotificationsService interface {
	StartMaintenance()
	Create(w http.ResponseWriter, r *http.Request)
	List(w http.ResponseWriter, r *http.Request)
	TypesList(w http.ResponseWriter, r *http.Request)
	TypesPatch(w http.ResponseWriter, r *http.Request)
	Read(w http.ResponseWriter, r *http.Request)
	ReadAll(w http.ResponseWriter, r *http.Request)
	Delete(w http.ResponseWriter, r *http.Request)
	BulkDelete(w http.ResponseWriter, r *http.Request)
	Stats(w http.ResponseWriter, r *http.Request)
	Stream(w http.ResponseWriter, r *http.Request)
	Get(w http.ResponseWriter, r *http.Request)
	Attachment(w http.ResponseWriter, r *http.Request)
}

// FilesService 是文件区与通用上传下载域（internal/files 实现）。
type FilesService interface {
	Upload(w http.ResponseWriter, r *http.Request)
	FilesUpload(w http.ResponseWriter, r *http.Request)
	FilesList(w http.ResponseWriter, r *http.Request)
	FilesDownload(w http.ResponseWriter, r *http.Request)
	FilesMkdir(w http.ResponseWriter, r *http.Request)
	FilesRename(w http.ResponseWriter, r *http.Request)
	FilesDelete(w http.ResponseWriter, r *http.Request)
	Download(w http.ResponseWriter, r *http.Request)
}

// SharesService 是文件区临时链接域（internal/shares 实现）。
type SharesService interface {
	Create(w http.ResponseWriter, r *http.Request)
	List(w http.ResponseWriter, r *http.Request)
	Update(w http.ResponseWriter, r *http.Request)
	Delete(w http.ResponseWriter, r *http.Request)
	PublicDownload(w http.ResponseWriter, r *http.Request)
}

// Options 是装配参数。
type Options struct {
	Cfg     *config.Config
	Files   *config.FileStore
	Redis   *state.Client
	Log     *slog.Logger
	Metrics MetricsService
	Apps    AppsService
	Notifs  NotificationsService
	FileSvc FilesService
	Shares  SharesService
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
	notifs  NotificationsService
	fileSvc FilesService
	shares  SharesService
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
		notifs:      opts.Notifs,
		fileSvc:     opts.FileSvc,
		shares:      opts.Shares,
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
	for i := range s.routes {
		rt := &s.routes[i]
		params, ok := matchRoute(rt, req)
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
		case authNotificationsWrite:
			s.auth.NotificationsWrite(rt.handler).ServeHTTP(w, routed)
		default:
			rt.handler.ServeHTTP(w, routed)
		}
		return
	}
	s.notFound(w, req)
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
	if r.Method == http.MethodPost && strings.TrimRight(r.URL.Path, "/") == "/api/admin/notifications" {
		limit = 24 * 1024 * 1024
	}
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
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	msg := "Cannot " + r.Method + " " + r.URL.Path
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>"+escapeHTML(msg)+"</pre>\n</body>\n</html>\n")
}

func escapeHTML(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return replacer.Replace(s)
}
