// Package auth 实现管理端鉴权：builtin 会话（Redis + cookie）、sso 头、接口令牌，
// 以及 TOTP 动态码校验与 JWT 签发。语义逐条对齐旧实现 src/middleware/auth.ts 与 totp-auth 模块。
package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/state"
	"admin-server-go/internal/util"
)

// SessionTTL 与旧实现一致：12 小时，校验通过后滑动续期。
const SessionTTL = config.SessionTTL

const (
	// APITokenPrefix 是 Redis 接口令牌键前缀：只存 sha256 哈希，明文仅生成时返回一次。
	APITokenPrefix = "api:token:"
	// APITokenMaxDays 是接口令牌有效期上限（天）。
	APITokenMaxDays = 365
)

// apiTokenPrefix 是包内简称。
const apiTokenPrefix = APITokenPrefix

// APITokenMeta 是 Redis 里 api:token:<sha256> 值的结构。
type APITokenMeta struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Note       string `json:"note,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
	ExpiresAt  int64  `json:"expiresAt,omitempty"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
	CanWrite   bool   `json:"canWrite,omitempty"`
}

// Authenticator 持有鉴权所需依赖。
type Authenticator struct {
	Cfg   *config.Config
	Files *config.FileStore
	Redis *state.Client
	Log   *slog.Logger
}

// New 构造鉴权器。
func New(cfg *config.Config, files *config.FileStore, redis *state.Client, log *slog.Logger) *Authenticator {
	return &Authenticator{Cfg: cfg, Files: files, Redis: redis, Log: log}
}

// SessionKey 返回会话键。
func (a *Authenticator) SessionKey(token string) string { return a.Cfg.RedisPrefix + token }

// SessionPrefix 返回会话键前缀（用于列表/清理）。
func (a *Authenticator) SessionPrefix() string { return a.Cfg.RedisPrefix }

// BuiltinSessionToken 取自带账号会话 token：Authorization: Bearer 优先，其次 cookie admin_session。
func BuiltinSessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if t := strings.TrimSpace(h[len("Bearer "):]); t != "" {
			return t
		}
	}
	if c, err := r.Cookie("admin_session"); err == nil {
		return c.Value
	}
	return ""
}

// SetSessionCookie 下发会话 cookie（Path=/; HttpOnly; SameSite=Lax；HTTPS 下加 Secure）。
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := false
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto == "https" {
		secure = true
	}
	if r.TLS != nil {
		secure = true
	}
	parts := []string{
		"admin_session=" + token,
		"Path=/",
		"HttpOnly",
		"SameSite=Lax",
		"Max-Age=" + itoa(SessionTTL),
	}
	if secure {
		parts = append(parts, "Secure")
	}
	w.Header().Add("Set-Cookie", strings.Join(parts, "; "))
}

// ClearSessionCookie 清除会话 cookie。
func ClearSessionCookie(w http.ResponseWriter) {
	w.Header().Add("Set-Cookie", "admin_session=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0")
}

// Require 是业务鉴权中间件：按 AUTH_MODE 分支，失败一律 401 JSON（不 302、不 500）。
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Cfg.AuthMode == config.AuthBuiltin {
			token := BuiltinSessionToken(r)
			if token != "" {
				ctx := r.Context()
				v, err := a.Redis.Get(ctx, a.SessionKey(token))
				if err != nil {
					httpx.Error(w, http.StatusInternalServerError, "会话服务异常")
					return
				}
				if v != "" {
					_ = a.Redis.Expire(ctx, a.SessionKey(token), SessionTTL)
					next.ServeHTTP(w, httpx.WithUser(r, httpx.User{Role: "admin", Name: "admin", Via: "builtin"}))
					return
				}
			}
			httpx.Error(w, http.StatusUnauthorized, "未登录")
			return
		}
		if x := strings.TrimSpace(r.Header.Get("X-Auth-User")); x != "" {
			next.ServeHTTP(w, httpx.WithUser(r, httpx.User{Role: "admin", Name: x, Via: "gateway"}))
			return
		}
		httpx.Error(w, http.StatusUnauthorized, "未登录")
	})
}

// DirectLoopback 判断请求是否为本机直连（无任何反代注入头）。
func DirectLoopback(r *http.Request) bool {
	addr := r.RemoteAddr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	addr = strings.TrimPrefix(addr, "::ffff:")
	if addr != "127.0.0.1" && addr != "::1" {
		return false
	}
	if r.Header.Get("X-Real-IP") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	return true
}

// RequestAPIToken 从请求中提取接口令牌明文：Authorization Bearer、X-Quotahub-Token、?token=。
func RequestAPIToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if t := strings.TrimSpace(h[len("Bearer "):]); t != "" {
			return t
		}
	}
	if alias := strings.TrimSpace(r.Header.Get("X-Quotahub-Token")); alias != "" {
		return alias
	}
	if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
		return q
	}
	return ""
}

// LookupAPITokenMeta 按明文查令牌表；未命中/数据异常返回 nil。
func (a *Authenticator) LookupAPITokenMeta(ctx context.Context, token string) (*APITokenMeta, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := a.Redis.Get(ctx, apiTokenPrefix+util.Sha256Hex(token))
	if err != nil {
		return nil, err
	}
	return ParseAPITokenMeta(raw), nil
}

// ParseAPITokenMeta 解析 api:token: 值的 JSON；返回 nil 表示数据异常。
func ParseAPITokenMeta(raw string) *APITokenMeta {
	if raw == "" {
		return nil
	}
	var meta APITokenMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil
	}
	if meta.ID == "" || meta.Name == "" {
		return nil
	}
	return &meta
}

// APITokenKey 返回令牌键。
func APITokenKey(id string) string { return apiTokenPrefix + id }

// NotificationsWrite 是通知写入通道鉴权：直连回环免 SSO；否则可写令牌放行，
// 只读令牌 403，其它回落 authRequired。
func (a *Authenticator) NotificationsWrite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if DirectLoopback(r) {
			next.ServeHTTP(w, r)
			return
		}
		if token := RequestAPIToken(r); token != "" {
			meta, err := a.LookupAPITokenMeta(r.Context(), token)
			if err != nil {
				httpx.Error(w, http.StatusInternalServerError, "会话服务异常")
				return
			}
			if meta != nil {
				if meta.CanWrite {
					next.ServeHTTP(w, r)
					return
				}
				httpx.Error(w, http.StatusForbidden, "该接口令牌为只读，无权写入通知")
				return
			}
		}
		a.Require(next).ServeHTTP(w, r)
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
