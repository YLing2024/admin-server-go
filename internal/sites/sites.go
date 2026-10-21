// Package sites 提供「站点」页的三块只读数据：nginx server 块、Let's Encrypt 证书、
// 备份目录与 systemd timer。
//
// 采集全部走外部只读命令（nginx -T / openssl / du / systemctl）与文件系统读取，
// 不写任何配置、不触发续期、不清理备份。每个接口整体 10 秒超时，结果缓存 60 秒：
// 这些数据变化很慢，缓存可以避免每次刷新都 fork openssl/du。
// 任一子命令失败只影响对应字段：接口仍返回 200，数组为空并带 error 文案。
package sites

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"admin-server-go/internal/httpx"
)

const (
	// cacheTTL 是三个接口共用的结果缓存时长。
	cacheTTL = 60 * time.Second
	// collectTimeout 是单次采集（含全部子命令）的上限。
	collectTimeout = 10 * time.Second
	// backupLimit 是备份列表最多返回的条目数（按修改时间倒序后截断）。
	backupLimit = 50
)

// runner 执行一条只读命令并返回 stdout；stderr 并入错误文案。
// 抽成字段是为了让单测能替换成固定输出，不依赖宿主机上的 nginx/openssl。
type runner func(ctx context.Context, name string, args ...string) (string, error)

// Options 是站点服务的装配参数。路径为空时使用生产默认值。
type Options struct {
	Log       *slog.Logger
	NginxBin  string
	CertDir   string
	BackupDir string
}

// Service 是「站点」页的数据服务。三个接口各自持有一份结果缓存。
type Service struct {
	log       *slog.Logger
	nginxBin  string
	certDir   string
	backupDir string
	run       runner
	now       func() time.Time

	nginxCache   timedCache[NginxPayload]
	certsCache   timedCache[CertsPayload]
	backupsCache timedCache[BackupsPayload]
}

// NginxSite 是一个 nginx server 块的摘要。
type NginxSite struct {
	ServerName string   `json:"serverName"`
	Listen     []string `json:"listen"`
	Root       *string  `json:"root"`
	ProxyPass  *string  `json:"proxyPass"`
	CertPath   *string  `json:"certPath"`
	ConfigFile string   `json:"configFile"`
	Redirect   *string  `json:"redirect"`
	IsRegex    bool     `json:"isRegex"`
}

// NginxPayload 是 GET /api/admin/sites/nginx 的响应体。
type NginxPayload struct {
	Sites []NginxSite `json:"sites"`
	Error string      `json:"error,omitempty"`
}

// Cert 是一张证书的摘要；单个证书解析失败时 Error 有值，其余条目不受影响。
type Cert struct {
	Domain    string  `json:"domain"`
	NotAfter  *string `json:"notAfter"`
	DaysLeft  *int    `json:"daysLeft"`
	Issuer    string  `json:"issuer"`
	AutoRenew bool    `json:"autoRenew"`
	Error     string  `json:"error,omitempty"`
}

// CertsPayload 是 GET /api/admin/sites/certs 的响应体。
type CertsPayload struct {
	Certs []Cert `json:"certs"`
	Error string `json:"error,omitempty"`
}

// Backup 是 /data/backups 下的一个一级目录。
type Backup struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modifiedAt"`
}

// BackupTimer 是名字含 backup 的 systemd timer 的两个关键属性。
type BackupTimer struct {
	Name                   string `json:"name"`
	NextElapseUSecRealtime string `json:"nextElapseUSecRealtime"`
	LastTriggerUSec        string `json:"lastTriggerUSec"`
}

// BackupsPayload 是 GET /api/admin/sites/backups 的响应体。
// Total / TotalSize / LatestModifiedAt 统计的是截断前的全部一级目录，
// 这样前端摘要不会因为只展示 50 条而少算。
type BackupsPayload struct {
	Backups          []Backup      `json:"backups"`
	Timers           []BackupTimer `json:"timers"`
	Total            int           `json:"total"`
	TotalSize        int64         `json:"totalSize"`
	LatestModifiedAt int64         `json:"latestModifiedAt"`
	Error            string        `json:"error,omitempty"`
}

// New 构造站点数据服务。
func New(opts Options) *Service {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		log:       log,
		nginxBin:  firstNonEmpty(opts.NginxBin, "nginx"),
		certDir:   firstNonEmpty(opts.CertDir, "/etc/letsencrypt/live"),
		backupDir: firstNonEmpty(opts.BackupDir, "/data/backups"),
		now:       time.Now,
	}
	s.run = runCommand
	return s
}

// Nginx 处理 GET /api/admin/sites/nginx。
func (s *Service) Nginx(w http.ResponseWriter, _ *http.Request) {
	payload := s.nginxCache.get(s.now(), cacheTTL, s.collectNginx)
	if payload.Sites == nil {
		payload.Sites = []NginxSite{}
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}

// Certs 处理 GET /api/admin/sites/certs。
func (s *Service) Certs(w http.ResponseWriter, _ *http.Request) {
	payload := s.certsCache.get(s.now(), cacheTTL, s.collectCerts)
	if payload.Certs == nil {
		payload.Certs = []Cert{}
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}

// Backups 处理 GET /api/admin/sites/backups。
func (s *Service) Backups(w http.ResponseWriter, _ *http.Request) {
	payload := s.backupsCache.get(s.now(), cacheTTL, s.collectBackups)
	if payload.Backups == nil {
		payload.Backups = []Backup{}
	}
	if payload.Timers == nil {
		payload.Timers = []BackupTimer{}
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}

// timedCache 是带过期的单值缓存。持锁加载，避免并发首次请求重复 fork 子命令。
type timedCache[T any] struct {
	mu    sync.Mutex
	value T
	at    time.Time
	valid bool
}

func (c *timedCache[T]) get(now time.Time, ttl time.Duration, load func(context.Context) T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && now.Sub(c.at) < ttl {
		return c.value
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	c.value = load(ctx)
	c.at = now
	c.valid = true
	return c.value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
