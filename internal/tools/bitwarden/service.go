// Package bitwarden 实现「工具」面板里的 Bitwarden 解锁：用主密码直接调用身份服务换
// access_token，原子写入 0600 的会话文件，供服务器上的 bw CLI 与 Hermes 使用。
//
// 隐私边界（需求书 §三）决定了这里刻意不做的几件事：
//   - 不记录密码，也不把上游错误/响应体写进日志（上游可能回显账号信息）；
//   - 不把 token 返回给前端，状态接口只给布尔值与剩余秒数；
//   - 失败一律回同一句「密码错误」，避免区分账号不存在 / 网络错误 / 上游 5xx 被用来探测；
//   - 写入用同目录临时文件 + rename，读方不会看到半截 token。
package bitwarden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

const (
	// name 是工具清单里的展示名。
	name = "Bitwarden 解锁"
	// requestTimeout 覆盖整次身份服务请求；上游卡住时不能拖住管理端请求。
	requestTimeout = 10 * time.Second
	// maxAttemptsPerMinute 是同 IP 的滑动窗口上限；超出后第 6 次直接 429。
	maxAttemptsPerMinute = 5
	// attemptWindow 是限速窗口。
	attemptWindow = time.Minute
	// maxResponseBytes 限制身份服务响应体，避免异常上游撑爆内存。
	maxResponseBytes = 1 << 20
)

// errAuthFailed 是统一的认证失败原因。调用方不得把它或上游响应细节回给客户端。
var errAuthFailed = errors.New("bitwarden 认证失败")

// Options 是装配参数。Client 与 Now 留出测试注入点，生产使用默认值。
type Options struct {
	Cfg *config.Config
	// Client 为空时使用带 10s 超时、禁止跟随重定向的默认客户端。
	Client *http.Client
	// Now 为空时使用 time.Now。
	Now func() time.Time
}

// Service 提供 Bitwarden 工具接口。限速状态只存在进程内（不引 Redis）。
type Service struct {
	cfg    *config.Config
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	attempts map[string][]time.Time
}

// Tool 是 GET /api/admin/tools 的单项。ExpiresAt 是 epoch 毫秒；token 不是 JWT 时为 null。
type Tool struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Unlocked   bool   `json:"unlocked"`
	ExpiresAt  *int64 `json:"expiresAt"`
}

// Status 是 GET /api/admin/tools/bitwarden/status 的响应。
// ExpiresInSeconds / UpdatedAt 为 null 表示读不出（非 JWT / 文件不存在），不是错误。
type Status struct {
	Configured       bool   `json:"configured"`
	Unlocked         bool   `json:"unlocked"`
	UpdatedAt        *int64 `json:"updatedAt"`
	ExpiresInSeconds *int64 `json:"expiresInSeconds"`
}

// sessionState 是会话文件的内部快照。
type sessionState struct {
	unlocked  bool
	updatedAt *int64
	expiresAt *int64
	expiresIn *int64
}

// New 构造服务。
func New(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{
			Timeout: requestTimeout,
			// 身份服务不应重定向；禁止跟随可避免把表单里的密码转发到别的地址。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Service{
		cfg:      opts.Cfg,
		client:   client,
		now:      now,
		attempts: map[string][]time.Time{},
	}
}

// configured 判断三项必填配置是否齐全；会话文件有默认值，不参与判定。
func (s *Service) configured() bool {
	return s.cfg != nil &&
		s.cfg.BitwardenServer != "" &&
		s.cfg.BitwardenEmail != "" &&
		s.cfg.BitwardenDeviceID != ""
}

// List 处理 GET /api/admin/tools：目前只有 Bitwarden 一项，数组形状便于后续追加工具。
func (s *Service) List(w http.ResponseWriter, _ *http.Request) {
	state := s.readSession()
	httpx.WriteJSON(w, http.StatusOK, []Tool{{
		ID:         "bitwarden",
		Name:       name,
		Configured: s.configured(),
		Unlocked:   state.unlocked,
		ExpiresAt:  state.expiresAt,
	}})
}

// Status 处理 GET /api/admin/tools/bitwarden/status：只回布尔与时间，绝不回 token。
func (s *Service) Status(w http.ResponseWriter, _ *http.Request) {
	if !s.configured() {
		httpx.Error(w, http.StatusNotImplemented, "未配置 BITWARDEN_*")
		return
	}
	state := s.readSession()
	httpx.WriteJSON(w, http.StatusOK, Status{
		Configured:       true,
		Unlocked:         state.unlocked,
		UpdatedAt:        state.updatedAt,
		ExpiresInSeconds: state.expiresIn,
	})
}

// Unlock 处理 POST /api/admin/tools/bitwarden/unlock。
// 密码只在本次请求的内存里存在：不写日志、不进审计、不返回。
func (s *Service) Unlock(w http.ResponseWriter, r *http.Request) {
	if !s.configured() {
		httpx.Error(w, http.StatusNotImplemented, "未配置 BITWARDEN_*")
		return
	}
	ip := util.ClientIP(r)
	if retryAfter, ok := s.allowAttempt(ip); !ok {
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":         false,
			"error":      "尝试过多，请稍后再试",
			"retryAfter": retryAfter,
		})
		return
	}

	password := httpx.BodyString(httpx.Body(r), "password")
	if password == "" {
		unlockFailed(w)
		return
	}
	token, err := s.exchange(r.Context(), password)
	if err != nil || token == "" {
		unlockFailed(w)
		return
	}
	if err := writeSessionFile(s.cfg.BitwardenSessionFile, token); err != nil {
		// 落盘失败也不区分原因：对客户端仍是同一句，避免泄漏文件系统状态。
		unlockFailed(w)
		return
	}
	state := s.readSession()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"expiresInSeconds": state.expiresIn,
	})
}

// Lock 处理 POST /api/admin/tools/bitwarden/lock：删除会话文件即失效；文件本就不存在也算成功。
func (s *Service) Lock(w http.ResponseWriter, _ *http.Request) {
	if !s.configured() {
		httpx.Error(w, http.StatusNotImplemented, "未配置 BITWARDEN_*")
		return
	}
	if err := os.Remove(s.cfg.BitwardenSessionFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "清除会话失败"})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// unlockFailed 统一认证失败响应：401 + 固定文案，不附带任何上游细节。
func unlockFailed(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "密码错误"})
}

// exchange 调 POST {SERVER}/identity/connect/token 换取 access_token。
// 任何失败都折叠成 errAuthFailed，调用方不需要（也不应该）区分原因。
func (s *Service) exchange(ctx context.Context, password string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", s.cfg.BitwardenEmail)
	form.Set("password", password)
	form.Set("scope", "api offline_access")
	form.Set("client_id", "cli")
	form.Set("deviceType", "10")
	form.Set("deviceIdentifier", s.cfg.BitwardenDeviceID)
	form.Set("deviceName", "admin-tools")

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	endpoint := strings.TrimRight(s.cfg.BitwardenServer, "/") + "/identity/connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errAuthFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", errAuthFailed
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return "", errAuthFailed
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", errAuthFailed
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.AccessToken) == "" {
		return "", errAuthFailed
	}
	return payload.AccessToken, nil
}

// allowAttempt 是进程内滑动窗口限速：记录每次解锁尝试（成功与否都算），
// 窗口内第 6 次返回 false 与建议重试秒数。map 按 IP 增长，但只在请求该 IP 时清理过期项。
func (s *Service) allowAttempt(ip string) (int, bool) {
	now := s.now()
	cutoff := now.Add(-attemptWindow)
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.attempts[ip]
	kept := list[:0]
	for _, t := range list {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= maxAttemptsPerMinute {
		retry := int(kept[0].Add(attemptWindow).Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		s.attempts[ip] = kept
		return retry, false
	}
	kept = append(kept, now)
	s.attempts[ip] = kept
	return 0, true
}

// readSession 读取会话文件并解析 JWT exp。读不到文件按「已锁定」处理；
// 非 JWT 按需求书 §一.5 视为已解锁但有效期未知（不报错）。
func (s *Service) readSession() sessionState {
	if s.cfg == nil || s.cfg.BitwardenSessionFile == "" {
		return sessionState{}
	}
	info, statErr := os.Stat(s.cfg.BitwardenSessionFile)
	raw, readErr := os.ReadFile(s.cfg.BitwardenSessionFile)
	if readErr != nil {
		return sessionState{}
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return sessionState{}
	}
	state := sessionState{unlocked: true}
	if statErr == nil {
		ms := info.ModTime().UnixMilli()
		state.updatedAt = &ms
	}
	exp, ok := jwtExpiry(token)
	if !ok {
		return state
	}
	expMs := exp * 1000
	state.expiresAt = &expMs
	remaining := exp - s.now().Unix()
	if remaining < 0 {
		remaining = 0
	}
	state.expiresIn = &remaining
	if remaining == 0 {
		// 文件还在但 token 已过期：对调用方等同未解锁，便于前端提示重新解锁。
		state.unlocked = false
	}
	return state
}

// jwtExpiry 从 JWT 的第二段取 exp（秒）；不是 JWT 或缺少 exp 时返回 false。
func jwtExpiry(token string) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return 0, false
	}
	return claims.Exp, true
}

// writeSessionFile 用同目录临时文件 + rename 原子替换；CreateTemp 默认 0600，
// 这里仍显式 Chmod，防止 umask / 平台差异把权限放宽。
func writeSessionFile(path, token string) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".bw_session-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := io.WriteString(f, token); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
