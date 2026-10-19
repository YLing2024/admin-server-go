package apps

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/probe"
	"admin-server-go/internal/util"
)

// Options 是构造应用面板服务的依赖。
//
// Probe 缺省时内部自建一个：探活只需无状态的 TCP 连通判断，独立实例不会与系统监控互相干扰。
type Options struct {
	Cfg   *config.Config
	Log   *slog.Logger
	Probe *probe.Service
}

// Service 是应用面板服务实例。
//
// 公开接口持有一个采集器（10s 缓存 + 单飞）；登记表解析结果按 mtime 缓存在 regCache / regMu 下。
// tcpProbe 是注入的本地端口探活闭包。
type Service struct {
	cfg *config.Config
	log *slog.Logger

	tcpProbe tcpChecker

	publicC *collector
	// refresh 是公开接口的刷新限流：按来源 IP 每 10s 至多真正刷新一次。
	refresh *RefreshLimiter

	regMu    sync.Mutex
	regCache registryCache
}

// New 构造应用面板服务。
func New(opts Options) *Service {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	pr := opts.Probe
	if pr == nil {
		pr = probe.New(opts.Cfg, log)
	}
	s := &Service{
		cfg:     opts.Cfg,
		log:     log,
		refresh: NewRefreshLimiter(refreshWindowMs),
	}
	// 单条探活统一 1500ms 超时，与登记表内的探活约定一致。
	s.tcpProbe = func(port int) bool { return pr.CheckTCPPort(port, probeTimeoutMs) }
	s.publicC = newCollector(s)
	return s
}

// isRefresh 判定 `?refresh=1` / `?refresh=true`（复刻旧实现 `req.query.refresh === '1' || 'true'`）。
func isRefresh(r *http.Request) bool {
	v := r.URL.Query().Get("refresh")
	return v == "1" || v == "true"
}

// elapsedMs 返回自 t0 起经过的毫秒数（审计用）。
func elapsedMs(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

// countDown 统计探活结果为 down 的条数（审计用，绝不读登记表 url）。
func countDown(apps []PublicAppView) int {
	n := 0
	for i := range apps {
		if apps[i].Status == StatusDown {
			n++
		}
	}
	return n
}

// PublicApps 处理 GET /api/public/apps（免鉴权）。
//
// `?refresh=1` 按来源 IP 限流，窗口内重复刷新按缓存读；只回字段白名单；响应头 Cache-Control: no-store；
// 失败一律 503 通用文案，不回栈信息。审计动作为 public_apps_list。
func (s *Service) PublicApps(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	t0 := time.Now()
	wantsRefresh := isRefresh(r)
	refresh := wantsRefresh && s.refresh.Allow(ip, time.Now().UnixMilli())
	payload, err := s.publicC.getPayload(refresh)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "public_apps_list", ip, false, fmt.Sprintf("ms=%d", elapsedMs(t0)))
		httpx.Error(w, http.StatusServiceUnavailable, "应用数据暂不可用")
		return
	}
	util.Audit(s.cfg.AuditLog, "public_apps_list", ip, true,
		fmt.Sprintf("apps=%d failed=%d ms=%d", len(payload.Apps), countDown(payload.Apps), elapsedMs(t0)))
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, payload)
}
