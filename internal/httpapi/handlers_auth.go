package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/auth"
	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

// ---- 本地登录失败锁定（内存按 IP；连续 5 次失败锁 60 秒）----

type loginLimiter struct {
	mu    sync.Mutex
	state map[string]*loginState
}

type loginState struct {
	count     int
	lockUntil int64
}

// touch 返回是否处于锁定与剩余秒数；锁定过期则清理。
func (l *loginLimiter) check(ip string, now int64) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		l.state = map[string]*loginState{}
	}
	rec := l.state[ip]
	if rec == nil {
		return false, 0
	}
	if rec.lockUntil != 0 {
		if rec.lockUntil > now {
			return true, int((rec.lockUntil - now + 999) / 1000)
		}
		delete(l.state, ip)
	}
	return false, 0
}

func (l *loginLimiter) fail(ip string, now int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		l.state = map[string]*loginState{}
	}
	rec := l.state[ip]
	if rec == nil {
		rec = &loginState{}
		l.state[ip] = rec
	}
	rec.count++
	if rec.count >= 5 {
		rec.lockUntil = now + 60*1000
	}
	return rec.count
}

func (l *loginLimiter) clear(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, ip)
}

// ---- 路由处理 ----

// handleRoot 返回服务标识。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": "admin-server", "status": "ok"})
}

// handleAuthMode 免鉴权地回当前认证模式。
func (s *Server) handleAuthMode(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"authMode": string(s.cfg.AuthMode)})
}

// handleLogin 用 TOTP 动态验证码登录，成功后写 Redis 会话并下发 cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	now := time.Now().UnixMilli()
	secret := s.auth.GetTotpSecret()
	if secret == "" {
		util.Audit(s.cfg.AuditLog, "login", ip, false, "TOTP 未配置")
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "首次使用：请先设置 TOTP", "code": "totp_setup_required"})
		return
	}
	if locked, remain := s.loginLimiter.check(ip, now); locked {
		util.Audit(s.cfg.AuditLog, "login", ip, false, "锁定中（剩余 "+itoa(remain)+"s）")
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":      "尝试过多，请 " + itoa(remain) + " 秒后再试",
			"code":       "rate_limited",
			"retryAfter": remain,
		})
		return
	}
	code := httpx.BodyString(httpx.Body(r), "code")
	if !auth.VerifyTotp(secret, strings.TrimSpace(code)) {
		count := s.loginLimiter.fail(ip, now)
		util.Audit(s.cfg.AuditLog, "login", ip, false, "验证码错误（第 "+itoa(count)+"/5 次）")
		httpx.Error(w, http.StatusUnauthorized, "验证码错误")
		return
	}
	s.loginLimiter.clear(ip)
	util.Audit(s.cfg.AuditLog, "login", ip, true, "ok")
	token, err := randomHex(32)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "login", ip, false, "会话服务异常")
		httpx.Error(w, http.StatusInternalServerError, "会话服务异常")
		return
	}
	if err := s.redis.Set(r.Context(), s.auth.SessionKey(token), token, auth.SessionTTL); err != nil {
		util.Audit(s.cfg.AuditLog, "login", ip, false, "会话服务异常")
		httpx.Error(w, http.StatusInternalServerError, "会话服务异常")
		return
	}
	auth.SetSessionCookie(w, r, token)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token})
}

// handleLogout 删除会话 + 清 cookie，幂等。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.BuiltinSessionToken(r); token != "" {
		_ = s.redis.Del(r.Context(), s.auth.SessionKey(token))
	}
	auth.ClearSessionCookie(w)
	util.Audit(s.cfg.AuditLog, "logout", util.ClientIP(r), true, "ok")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe 返回当前登录身份。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := httpx.UserFrom(r)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": u.Name, "role": u.Role})
}

// handleTotpSetup 首次设置 TOTP（secret 已配置则 409）。
func (s *Server) handleTotpSetup(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	if s.auth.GetTotpSecret() != "" {
		util.Audit(s.cfg.AuditLog, "totp_setup", ip, false, "ok")
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{"code": "totp_already_setup", "message": "TOTP already configured"})
		return
	}
	secret, err := auth.GenerateTotpSecret()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "生成 TOTP 密钥失败")
		return
	}
	if err := s.auth.SaveTotpSecret(secret); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "保存 TOTP 密钥失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "totp_setup", ip, true, "ok")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"secret": secret, "otpauthUri": auth.OtpauthURI(secret)})
}

// handleTotpLogin 是 totp-auth 模块的 /login：校验 TOTP 并签发 JWT（分档限速）。
func (s *Server) handleTotpLogin(w http.ResponseWriter, r *http.Request) {
	ip := remoteAddr(r)
	if locked, remain := s.totpLimiter.Status(ip); locked {
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"code": "rate_limited", "message": "Too many failed attempts", "retryAfter": remain,
		})
		return
	}
	secret := s.auth.GetTotpSecret()
	if secret == "" {
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"code": "totp_setup_required", "message": "TOTP is not configured yet"})
		return
	}
	code := strings.TrimSpace(httpx.BodyString(httpx.Body(r), "code"))
	if !auth.VerifyTotp(secret, code) {
		s.totpLimiter.RecordFailure(ip)
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"code": "invalid_code", "message": "Invalid TOTP code"})
		return
	}
	s.totpLimiter.RecordSuccess(ip)
	token, err := auth.SignJWTExpiresIn(map[string]any{"sub": totpIssuerConst, "ip": ip}, s.files.GetOrCreateJWTSecret(), "12h")
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "expiresIn": "12h"})
}

// handlePassword 修改管理密码：校验旧口令 → 更新 config.json → 清空所有会话。
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	body := httpx.Body(r)
	oldPw := httpx.BodyString(body, "old_password")
	newPw := httpx.BodyString(body, "new_password")
	if !s.files.VerifyAdminPassword(oldPw) {
		util.Audit(s.cfg.AuditLog, "password", ip, false, "旧密码错误")
		httpx.Error(w, http.StatusBadRequest, "旧密码错误")
		return
	}
	if _, ok := body["new_password"].(string); !ok || len(newPw) < 6 {
		httpx.Error(w, http.StatusBadRequest, "新密码至少 6 位")
		return
	}
	if err := s.files.SetAdminPassword(newPw); err != nil {
		util.Audit(s.cfg.AuditLog, "password", ip, false, "会话服务异常")
		httpx.Error(w, http.StatusInternalServerError, "会话服务异常")
		return
	}
	util.Audit(s.cfg.AuditLog, "password", ip, true, "修改密码")
	keys, err := s.redis.Keys(r.Context(), s.auth.SessionPrefix()+"*")
	if err == nil && len(keys) > 0 {
		_ = s.redis.Del(r.Context(), keys...)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "密码已修改，请重新登录"})
}

// ---- 转发认证中心 ----

var authCenterClient = &http.Client{Timeout: 5 * time.Second}

func (s *Server) authCenterBaseURL() string {
	if s.cfg.AuthCenterBaseURL != "" {
		return s.cfg.AuthCenterBaseURL
	}
	return strings.TrimRight(s.files.Str("auth_center_base_url"), "/")
}

func (s *Server) readInternalToken() (string, error) {
	raw, err := os.ReadFile(s.cfg.AuthCenterInternalTokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *Server) forwardTotp(action string) Handler {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := util.ClientIP(r)
		sub := forwardedSub(r)
		if sub == "" {
			util.Audit(s.cfg.AuditLog, "totp_"+action, ip, false, "缺少 X-Auth-User")
			httpx.Error(w, http.StatusUnauthorized, "未登录")
			return
		}
		base := s.authCenterBaseURL()
		if base == "" {
			util.Audit(s.cfg.AuditLog, "totp_"+action, ip, false, "认证中心地址未配置")
			httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
			return
		}
		token, err := s.readInternalToken()
		if err != nil {
			util.Audit(s.cfg.AuditLog, "totp_"+action, ip, false, "内部令牌文件不可读")
			httpx.Error(w, http.StatusInternalServerError, "服务未正确配置")
			return
		}
		if token == "" {
			util.Audit(s.cfg.AuditLog, "totp_"+action, ip, false, "内部令牌文件为空")
			httpx.Error(w, http.StatusInternalServerError, "服务未正确配置")
			return
		}
		body, _ := json.Marshal(httpx.Body(r))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
			base+"/api/internal/totp/"+action+"?sub="+urlQueryEscape(sub), strings.NewReader(string(body)))
		if err != nil {
			httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
			return
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Token", token)
		resp, err := authCenterClient.Do(req)
		if err != nil {
			util.Audit(s.cfg.AuditLog, "totp_"+action, ip, false, "认证中心不可达: "+err.Error())
			httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
			return
		}
		defer resp.Body.Close()
		payload := readJSONBody(resp)
		util.Audit(s.cfg.AuditLog, "totp_"+action, ip, resp.StatusCode < 400, "内部接口")
		writeRawJSON(w, resp.StatusCode, payload)
	}
}

// forwardSessions 转发设备会话管理到认证中心内部接口。
func (s *Server) forwardSessions(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	action := strings.ToLower(r.Method)
	switch r.Method {
	case http.MethodGet:
		action = "list"
	case http.MethodPut:
		action = "rename"
	case http.MethodDelete:
		action = "delete"
	}
	sub := forwardedSub(r)
	base := s.authCenterBaseURL()
	if base == "" {
		util.Audit(s.cfg.AuditLog, "sessions_"+action, ip, false, "认证中心地址未配置")
		httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
		return
	}
	token, err := s.readInternalToken()
	if err != nil {
		util.Audit(s.cfg.AuditLog, "sessions_"+action, ip, false, "内部令牌文件不可读")
		httpx.Error(w, http.StatusInternalServerError, "服务未正确配置")
		return
	}
	if token == "" {
		util.Audit(s.cfg.AuditLog, "sessions_"+action, ip, false, "内部令牌文件为空")
		httpx.Error(w, http.StatusInternalServerError, "服务未正确配置")
		return
	}
	relPath := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions")
	if relPath == "/" {
		relPath = ""
	}
	target := base + "/api/internal/sessions" + relPath + "?sub=" + urlQueryEscape(sub)
	var bodyReader io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		raw, _ := json.Marshal(httpx.Body(r))
		bodyReader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
	if err != nil {
		httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
		return
	}
	req.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Internal-Token", token)
	resp, err := authCenterClient.Do(req)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "sessions_"+action, ip, false, "认证中心不可达: "+err.Error())
		httpx.Error(w, http.StatusBadGateway, "认证中心不可达")
		return
	}
	defer resp.Body.Close()
	payload := readJSONBody(resp)
	util.Audit(s.cfg.AuditLog, "sessions_"+action, ip, resp.StatusCode < 400, "内部接口")
	writeRawJSON(w, resp.StatusCode, payload)
}

func forwardedSub(r *http.Request) string {
	if u, ok := httpx.UserFrom(r); ok && u.Name != "" {
		return u.Name
	}
	return strings.TrimSpace(r.Header.Get("X-Auth-User"))
}

func remoteAddr(r *http.Request) string {
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		addr = addr[:idx]
	}
	return addr
}

func readJSONBody(resp *http.Response) json.RawMessage {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return json.RawMessage("{}")
	}
	if !json.Valid(data) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(data)
}

func writeRawJSON(w http.ResponseWriter, status int, payload json.RawMessage) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(urlEncode(s), "+", "%20"), "%2F", "%2F")
}

func urlEncode(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte("0123456789ABCDEF"[c>>4])
			b.WriteByte("0123456789ABCDEF"[c&0x0f])
		}
	}
	return b.String()
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
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

// totpIssuerConst 与旧实现一致（JWT sub 与 otpauth 标签）。
const totpIssuerConst = "HomeAdmin"

// 确保 config 被引用（SessionTTL 等常量来自 config）。
var _ = config.SessionTTL
