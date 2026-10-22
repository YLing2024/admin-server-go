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
	"os"
	"os/exec"
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

// errCLINotLoggedIn 表示 bw CLI 在本进程环境里找不到登录态（典型的 HOME 缺失）。
// 必须与「密码错误」区分开：否则运维会被一个看似密码问题、实为环境问题的报错误导很久。
var errCLINotLoggedIn = errors.New("bw CLI 未登录")

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
	// URL 非空表示这是一个「外链工具」：点开直接跳转，没有服务端表单。
	URL string `json:"url,omitempty"`
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

// List 处理 GET /api/admin/tools：Bitwarden 解锁 + 私密信箱（外链工具）。
// 数组形状便于后续继续追加。
func (s *Service) List(w http.ResponseWriter, _ *http.Request) {
	state := s.readSession()
	httpx.WriteJSON(w, http.StatusOK, []Tool{
		{
			ID:         "bitwarden",
			Name:       name,
			Configured: s.configured(),
			Unlocked:   s.configured() && state.unlocked,
			ExpiresAt:  state.expiresAt,
		},
		{
			ID:         "inbox",
			Name:       "私密信箱",
			Configured: true,
			Unlocked:   true,
			URL:        "https://drop.zhangyunling.cn/pair/883ac6c23e6f1492cb84a4fe85715ab03aefcd7dd3279b519c1556b8ef0afb87",
		},
	})
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
	if errors.Is(err, errCLINotLoggedIn) {
		// 环境问题，不是密码问题：给出可执行的提示，避免继续误判成密码错误。
		httpx.WriteJSON(w, http.StatusConflict, map[string]any{
			"ok":    false,
			"error": "bw CLI 未登录：服务缺少 HOME 或登录态已丢失，需运维处理",
		})
		return
	}
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

// Lock 处理 POST /api/admin/tools/bitwarden/lock：删会话文件并让 CLI 真正上锁；
// 只删文件不够 —— bw 自己不落锁的话，内存里仍能读条目，界面会误报「已锁定」。
func (s *Service) Lock(w http.ResponseWriter, _ *http.Request) {
	if !s.configured() {
		httpx.Error(w, http.StatusNotImplemented, "未配置 BITWARDEN_*")
		return
	}
	// CLI 上锁失败不影响本次响应：文件删除才是对外可见的锁定语义。
	_ = exec.Command("bw", "lock").Run()
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

// exchange 用主密码换出会话令牌。
//
// 走官方 bw CLI 而不是直接打 /identity/connect/token：该接口收的是客户端派生出的
// master password hash，不是明文主密码（早期版本直接发明文，导致「密码正确却提示
// 密码错误」）。KDF 参数由账号决定，交给 CLI 才不会随参数变化而失效。
func (s *Service) exchange(ctx context.Context, password string) (string, error) {
	return s.unlockViaCLI(ctx, password)
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
