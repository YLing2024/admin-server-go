// Package config 负责 admin-server-go 的环境变量解析与 config.json 读写。
//
// 口径逐项对齐旧实现 src/config.ts 与 src/db.ts：环境变量同名，取值非法/未设置一律回退默认；
// AUTH_MODE 非法回落 builtin。与旧实现的有意差异都属于「新仓库必须与旧实例隔离」的部署约定
// （需求书 §5）：默认 PORT 改为 19265，Redis 前缀 / 采样锁 / 历史文件 / 密钥文件默认指向本仓库
// 自己的位置，避免与线上 3100 实例互相干扰。存储自 2026-10-16 起一律 MySQL（无方言开关），
// 指标库名默认即云库名。差异逐条记在 docs/DB.md。
package config

import (
	"fmt"
	"log/slog"
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

// TermUnlockMode 是终端口令门开关（ADMIN_TERM_UNLOCK）。管理端鉴权是两道门里的第一道，
// 本开关只决定第二道（终端口令）是否保留，不改变任何 HTTP 契约。
type TermUnlockMode string

const (
	// TermUnlockPassword 要求终端口令换取票据（默认，与旧实现一致）。
	TermUnlockPassword TermUnlockMode = "password"
	// TermUnlockOff 不要求终端口令：通过管理端鉴权后直接签发票据。
	TermUnlockOff TermUnlockMode = "off"
)

// Config 是启动时确定的运行时配置快照（不含 config.json 的可变内容，后者见 FileStore）。
type Config struct {
	// RepoRoot 是默认相对路径的解析基准（进程工作目录，与旧实现 __dirname/.. 等价）。
	RepoRoot string

	Host     string
	Port     int
	AuthMode AuthMode

	ConfigPath string
	AppsFile   string
	AuditLog   string
	TermPwFile string
	// TermUnlock 决定 POST /api/admin/term/unlock 是否校验终端口令（默认 password）。
	TermUnlock TermUnlockMode

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

	// 数据库连接参数（仅 MySQL）。指标库 schema 由 DB_METRICS_NAME 指定（默认即云库名）。
	DBHost        string
	DBPort        int
	DBUser        string
	DBPassword    string
	DBMetricsName string
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

	procMaxRows := 2000
	if raw := strings.TrimSpace(get("ADMIN_PROC_MAX_ROWS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			procMaxRows = n
		}
	}

	tmp := os.TempDir()
	cfg := &Config{
		RepoRoot:                    repoRoot,
		Host:                        host,
		Port:                        port,
		AuthMode:                    authMode,
		ConfigPath:                  trimOr(get("ADMIN_CONFIG_PATH"), filepath.Join(repoRoot, "config.json")),
		AppsFile:                    trimOr(get("ADMIN_APPS_FILE"), filepath.Join(repoRoot, "data", "apps.json")),
		AuditLog:                    trimOr(get("ADMIN_AUDIT_LOG"), filepath.Join(repoRoot, "audit.log")),
		TermPwFile:                  trimOr(get("ADMIN_TERM_PW_FILE"), filepath.Join(os.Getenv("HOME"), ".hermes", "term_password")),
		TermUnlock:                  parseTermUnlock(get("ADMIN_TERM_UNLOCK")),
		RedisURL:                    trimOr(get("REDIS_URL"), "redis://127.0.0.1:6379"),
		RedisPrefix:                 trimOr(get("ADMIN_REDIS_PREFIX"), "admin-go:session:"),
		SamplerLock:                 trimOr(get("ADMIN_SAMPLER_LOCK"), filepath.Join(tmp, "admin-server-go-sampler.lock")),
		HistoryFile:                 trimOr(get("ADMIN_HISTORY_FILE"), filepath.Join(tmp, "admin-server-go-history.json")),
		TotpSecretFile:              trimOr(get("ADMIN_TOTP_SECRET_FILE"), filepath.Join(repoRoot, "data", "totp-secret.json")),
		HermesStateDB:               trimOr(get("HERMES_STATE_DB"), filepath.Join(os.Getenv("HOME"), ".hermes", "state.db")),
		AuthCenterBaseURL:           strings.TrimRight(strings.TrimSpace(get("AUTH_CENTER_BASE_URL")), "/"),
		AuthCenterInternalTokenFile: trimOr(get("AUTH_CENTER_INTERNAL_TOKEN_FILE"), filepath.Join(repoRoot, "..", "auth-server", "internal-token")),
		ProcMaxRows:                 procMaxRows,
	}

	db, err := loadDB(get)
	if err != nil {
		return nil, err
	}
	cfg.DBHost = db.host
	cfg.DBPort = db.port
	cfg.DBUser = db.user
	cfg.DBPassword = db.password
	cfg.DBMetricsName = db.metricsName
	return cfg, nil
}

type dbOptions struct {
	host        string
	port        int
	user        string
	password    string
	metricsName string
}

// loadDB 解析 MySQL 连接参数。指标库 schema 默认即云库名（admin_metrics），
// 因此常规部署只需给出 DB_HOST / DB_USER / DB_PASSWORD。
func loadDB(get func(string) string) (dbOptions, error) {
	host := trimOr(get("DB_HOST"), defaultDBHost)
	user := strings.TrimSpace(get("DB_USER"))
	if user == "" {
		return dbOptions{}, &ConfigError{Message: "缺少 DB_USER（MySQL 账号必填；连接参数见 docs/DB.md）"}
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
		host:        host,
		port:        port,
		user:        user,
		password:    get("DB_PASSWORD"),
		metricsName: trimOr(get("DB_METRICS_NAME"), defaultMetricsDBName),
	}, nil
}

func trimOr(raw, fallback string) string {
	if v := strings.TrimSpace(raw); v != "" {
		return v
	}
	return fallback
}

// parseTermUnlock 解析 ADMIN_TERM_UNLOCK（大小写不敏感）：缺省与 password 均保留口令门，
// off 关闭；非法值回落 password 并告警一次——不做 fatal，避免一个拼错的开关让进程起不来。
func parseTermUnlock(raw string) TermUnlockMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(TermUnlockPassword):
		return TermUnlockPassword
	case string(TermUnlockOff):
		return TermUnlockOff
	default:
		slog.Warn("ADMIN_TERM_UNLOCK 取值非法，回落为 password", "value", strings.TrimSpace(raw))
		return TermUnlockPassword
	}
}

// 数据库默认值：库名默认即云库名（唯一权威见 go-rewrite/docs/mysql-schema-cloud.md）。
const (
	// defaultDBHost 是未配置 DB_HOST 时的回环地址。
	defaultDBHost = "127.0.0.1"
	// defaultMetricsDBName 是指标库默认 schema。
	defaultMetricsDBName = "admin_metrics"
)

// SessionTTL 是 builtin 会话有效期（秒），与旧实现一致：12 小时，校验通过后滑动续期。
const SessionTTL = 12 * 3600

// NowMillis 是默认时钟（毫秒），便于在测试里统一替换。
func NowMillis() int64 { return time.Now().UnixMilli() }

// Describe 生成启动摘要（不含任何敏感值）。
func (c *Config) Describe() string {
	return strings.Join([]string{
		fmt.Sprintf("监听 http://%s:%d", c.Host, c.Port),
		"认证模式 " + string(c.AuthMode),
		"数据库 mysql " + c.DBMetricsName,
	}, " | ")
}
