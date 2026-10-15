// Package config 负责 admin-server-go 的环境变量解析与 config.json 读写。
//
// 口径逐项对齐旧实现 src/config.ts 与 src/db.ts：环境变量同名，取值非法/未设置一律回退默认；
// AUTH_MODE 非法回落 builtin。与旧实现的有意差异只有两处，且都属于「新仓库必须与旧实例隔离」
// 的部署约定（需求书 §5）：默认 PORT 改为 19265，Redis 前缀 / 采样锁 / 历史文件 / 密钥文件
// 默认指向本仓库自己的位置，避免与线上 3100 实例互相干扰。差异逐条记在 docs/DB.md。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// AuthMode 管理端认证模式。
type AuthMode string

const (
	// AuthBuiltin 自带账号 + TOTP 登录 + 会话 cookie（默认）。
	AuthBuiltin AuthMode = "builtin"
	// AuthSSO 只信任前置认证层注入的 X-Auth-User，自带登录入口一律 404。
	AuthSSO AuthMode = "sso"
)

// DBDriver 数据库方言。
type DBDriver string

const (
	// DBDriverSQLite 两个独立的 modernc SQLite 文件（默认）。
	DBDriverSQLite DBDriver = "sqlite"
	// DBDriverMySQL 同一 MySQL 实例下的两个 schema，由 DB_METRICS_NAME / DB_NOTIFICATIONS_NAME 指定。
	DBDriverMySQL DBDriver = "mysql"
)

// Config 是启动时确定的运行时配置快照（不含 config.json 的可变内容，后者见 FileStore）。
type Config struct {
	// RepoRoot 是默认相对路径的解析基准（进程工作目录，与旧实现 __dirname/.. 等价）。
	RepoRoot string

	Host     string
	Port     int
	AuthMode AuthMode

	ConfigPath   string
	UploadDir    string
	FileDir      string
	AppsFile     string
	AuditLog     string
	TermPwFile   string
	ShareBaseURL string
	ShareFile    string

	MetricsDB            string
	NotificationsDB      string
	NotificationFilesDir string
	// NotificationRetentionSeconds 由 ADMIN_NOTIFICATION_RETENTION_DAYS 换算（默认 180 天）。
	NotificationRetentionDays    int
	NotificationRetentionSeconds int

	RedisURL    string
	RedisPrefix string
	SamplerLock string
	HistoryFile string

	TotpSecretFile string

	HermesStateDB string

	AuthCenterBaseURL           string
	AuthCenterInternalTokenFile string

	// ProcMaxRows 对齐 ADMIN_PROC_MAX_ROWS：进程排行最多返回的行数。
	ProcMaxRows int

	// 数据库选择与连接参数（本次新增；默认 sqlite，不受旧实现影响）。
	DBDriver            DBDriver
	DBDSN               string
	DBHost              string
	DBPort              int
	DBUser              string
	DBPassword          string
	DBMetricsName       string
	DBNotificationsName string
}

// ConfigError 表示无法用默认值兜底的配置错误（仅新增的数据库键会返回）。
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// Load 解析环境变量。get 便于测试注入。
func Load(get func(string) string) (*Config, error) {
	repoRoot := strings.TrimSpace(get("ADMIN_REPO_ROOT"))
	if repoRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			repoRoot = wd
		} else {
			repoRoot = "."
		}
	}

	port := 19265
	if raw := strings.TrimSpace(get("PORT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 65535 {
			port = n
		}
	}
	host := trimOr(get("HOST"), "0.0.0.0")

	authMode := AuthBuiltin
	if strings.EqualFold(strings.TrimSpace(get("AUTH_MODE")), "sso") {
		authMode = AuthSSO
	}

	retentionDays := 180
	if n, err := strconv.Atoi(strings.TrimSpace(get("ADMIN_NOTIFICATION_RETENTION_DAYS"))); err == nil && n > 0 {
		retentionDays = n
	}

	procMaxRows := 2000
	if raw := strings.TrimSpace(get("ADMIN_PROC_MAX_ROWS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			procMaxRows = n
		}
	}

	tmp := os.TempDir()
	cfg := &Config{
		RepoRoot:                     repoRoot,
		Host:                         host,
		Port:                         port,
		AuthMode:                     authMode,
		ConfigPath:                   trimOr(get("ADMIN_CONFIG_PATH"), filepath.Join(repoRoot, "config.json")),
		UploadDir:                    trimOr(get("ADMIN_UPLOAD_DIR"), filepath.Join(repoRoot, "uploads")),
		FileDir:                      trimOr(get("ADMIN_FILE_DIR"), "/root/files/download"),
		AppsFile:                     trimOr(get("ADMIN_APPS_FILE"), filepath.Join(repoRoot, "data", "apps.json")),
		AuditLog:                     trimOr(get("ADMIN_AUDIT_LOG"), filepath.Join(repoRoot, "audit.log")),
		TermPwFile:                   trimOr(get("ADMIN_TERM_PW_FILE"), filepath.Join(os.Getenv("HOME"), ".hermes", "term_password")),
		ShareBaseURL:                 strings.TrimRight(strings.TrimSpace(get("ADMIN_SHARE_BASE_URL")), "/"),
		ShareFile:                    trimOr(get("ADMIN_SHARE_FILE"), filepath.Join(repoRoot, "data", "file-shares.json")),
		MetricsDB:                    trimOr(get("ADMIN_METRICS_DB"), filepath.Join(repoRoot, "data", "metrics.db")),
		NotificationsDB:              trimOr(get("ADMIN_NOTIFICATIONS_DB"), filepath.Join(repoRoot, "data", "notifications.db")),
		NotificationRetentionDays:    retentionDays,
		NotificationRetentionSeconds: retentionDays * 24 * 3600,
		RedisURL:                     trimOr(get("REDIS_URL"), "redis://127.0.0.1:6379"),
		RedisPrefix:                  trimOr(get("ADMIN_REDIS_PREFIX"), "admin-go:session:"),
		SamplerLock:                  trimOr(get("ADMIN_SAMPLER_LOCK"), filepath.Join(tmp, "admin-server-go-sampler.lock")),
		HistoryFile:                  trimOr(get("ADMIN_HISTORY_FILE"), filepath.Join(tmp, "admin-server-go-history.json")),
		TotpSecretFile:               trimOr(get("ADMIN_TOTP_SECRET_FILE"), filepath.Join(repoRoot, "data", "totp-secret.json")),
		HermesStateDB:                trimOr(get("HERMES_STATE_DB"), filepath.Join(os.Getenv("HOME"), ".hermes", "state.db")),
		AuthCenterBaseURL:            strings.TrimRight(strings.TrimSpace(get("AUTH_CENTER_BASE_URL")), "/"),
		AuthCenterInternalTokenFile:  trimOr(get("AUTH_CENTER_INTERNAL_TOKEN_FILE"), filepath.Join(repoRoot, "..", "auth-server", "internal-token")),
		ProcMaxRows:                  procMaxRows,
	}
	cfg.NotificationFilesDir = trimOr(get("ADMIN_NOTIFICATION_FILES_DIR"), filepath.Join(filepath.Dir(cfg.NotificationsDB), "notification-files"))

	db, err := loadDB(get)
	if err != nil {
		return nil, err
	}
	cfg.DBDriver = db.driver
	cfg.DBDSN = db.dsn
	cfg.DBHost = db.host
	cfg.DBPort = db.port
	cfg.DBUser = db.user
	cfg.DBPassword = db.password
	cfg.DBMetricsName = db.metricsName
	cfg.DBNotificationsName = db.notifName
	return cfg, nil
}

type dbOptions struct {
	driver      DBDriver
	dsn         string
	host        string
	port        int
	user        string
	password    string
	metricsName string
	notifName   string
}

// loadDB 解析数据库选择。默认 sqlite；只有 DB_DRIVER=mysql 时才读取 MySQL 连接参数。
func loadDB(get func(string) string) (dbOptions, error) {
	raw := strings.ToLower(strings.TrimSpace(get("DB_DRIVER")))
	switch raw {
	case "", "sqlite":
		return dbOptions{driver: DBDriverSQLite}, nil
	case "mysql":
		dsn := strings.TrimSpace(get("DB_DSN"))
		if dsn != "" {
			names := strings.SplitN(dsn, "?", 2)[0]
			name := ""
			if i := strings.LastIndex(names, "/"); i >= 0 {
				name = names[i+1:]
			}
			return dbOptions{driver: DBDriverMySQL, dsn: dsn, metricsName: name, notifName: name}, nil
		}
		host := strings.TrimSpace(get("DB_HOST"))
		user := strings.TrimSpace(get("DB_USER"))
		metricsName := strings.TrimSpace(get("DB_METRICS_NAME"))
		notifName := strings.TrimSpace(get("DB_NOTIFICATIONS_NAME"))
		if host == "" || user == "" || metricsName == "" || notifName == "" {
			return dbOptions{}, &ConfigError{Message: "DB_DRIVER=mysql 需要 DB_HOST/DB_USER/DB_METRICS_NAME/DB_NOTIFICATIONS_NAME（或一条 DB_DSN）"}
		}
		port := 3306
		if p := strings.TrimSpace(get("DB_PORT")); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return dbOptions{}, &ConfigError{Message: fmt.Sprintf("DB_PORT 必须是 1–65535 的整数，当前为「%s」", p)}
			}
			port = n
		}
		return dbOptions{
			driver:      DBDriverMySQL,
			host:        host,
			port:        port,
			user:        user,
			password:    get("DB_PASSWORD"),
			metricsName: metricsName,
			notifName:   notifName,
		}, nil
	default:
		return dbOptions{}, &ConfigError{Message: fmt.Sprintf("DB_DRIVER 只能是 sqlite / mysql，当前为「%s」", raw)}
	}
}

func trimOr(raw, fallback string) string {
	if v := strings.TrimSpace(raw); v != "" {
		return v
	}
	return fallback
}

// SessionTTL 是 builtin 会话有效期（秒），与旧实现一致：12 小时，校验通过后滑动续期。
const SessionTTL = 12 * 3600

// NowMillis 是默认时钟（毫秒），便于在测试里统一替换。
func NowMillis() int64 { return time.Now().UnixMilli() }

// Describe 生成启动摘要（不含任何敏感值）。
func (c *Config) Describe() string {
	dbDesc := string(c.DBDriver)
	if c.DBDriver == DBDriverSQLite {
		dbDesc += " " + c.MetricsDB + " + " + c.NotificationsDB
	} else {
		dbDesc += " " + c.DBMetricsName + " + " + c.DBNotificationsName
	}
	return strings.Join([]string{
		fmt.Sprintf("监听 http://%s:%d", c.Host, c.Port),
		"认证模式 " + string(c.AuthMode),
		"数据库 " + dbDesc,
		"文件区 " + c.FileDir,
	}, " | ")
}
