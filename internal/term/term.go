// Package term 实现 Web 终端相关接口：口令二次验证 → 12h 票据签发 / 票据校验（仅本机）/
// ttyd(tmux) 会话列表与关闭。语义逐条对齐旧实现 src/routes/term.ts。
package term

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/state"
	"admin-server-go/internal/util"

	"log/slog"
)

const (
	// ticketPrefix 是 Redis 票据键前缀（只存 sha256(ticket)）。
	ticketPrefix = "term:ticket:"
	// ticketTTL 是票据有效期（秒）：12 小时。
	ticketTTL = 12 * 3600
	// unlockMaxFails / unlockLockout 是口令连续失败 5 次锁 10 分钟。
	unlockMaxFails = 5
	unlockLockout  = 10 * 60 * 1000
)

// sessionRe 是 tmux 会话名白名单：term- 前缀 + 小写字母/数字/短横，杜绝命令注入。
var sessionRe = regexp.MustCompile(`^term-[a-z0-9][a-z0-9-]{0,31}$`)

// Options 是装配参数。
type Options struct {
	Cfg   *config.Config
	Redis *state.Client
	Log   *slog.Logger
}

// Service 提供终端相关 handler。
type Service struct {
	cfg    *config.Config
	redis  *state.Client
	log    *slog.Logger
	mu     sync.Mutex
	unlock map[string]*unlockState
	now    func() time.Time
}

type unlockState struct {
	fails int
	until int64
}

// New 构造终端服务。
func New(opts Options) *Service {
	return &Service{cfg: opts.Cfg, redis: opts.Redis, log: opts.Log, unlock: map[string]*unlockState{}, now: time.Now}
}

// Unlock 校验终端口令，成功后下发 12h 票据。
func (s *Service) Unlock(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	now := s.now().UnixMilli()
	s.mu.Lock()
	st := s.unlock[ip]
	if st == nil {
		st = &unlockState{}
		s.unlock[ip] = st
	}
	if st.until > now {
		remain := int((st.until - now + 999) / 1000)
		s.mu.Unlock()
		httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过多，请稍后再试", "retryAfter": remain})
		return
	}
	body := httpx.Body(r)
	pw := httpx.BodyString(body, "password")
	if !s.passwordOK(pw) {
		st.fails++
		fails := st.fails
		if st.fails >= unlockMaxFails {
			st.until = now + unlockLockout
			st.fails = 0
		}
		s.mu.Unlock()
		util.Audit(s.cfg.AuditLog, "term_unlock_fail", ip, false, fmt.Sprintf("fails=%d", fails))
		httpx.Error(w, http.StatusUnauthorized, "口令不正确")
		return
	}
	st.fails = 0
	st.until = 0
	s.mu.Unlock()

	ticket, err := randomHex(32)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "票据存储失败: "+err.Error())
		return
	}
	if err := s.redis.Set(r.Context(), ticketPrefix+util.Sha256Hex(ticket), "1", ticketTTL); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "票据存储失败: "+err.Error())
		return
	}
	util.Audit(s.cfg.AuditLog, "term_unlock_ok", ip, true, "")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expiresIn": ticketTTL})
}

// Verify 供服务端 wrapper 起 shell 前调用；只允许本机来源。
func (s *Service) Verify(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	if !(ip == "127.0.0.1" || ip == "::1" || ip == "::ffff:127.0.0.1") {
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{"ok": false})
		return
	}
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"ok": false})
		return
	}
	v, err := s.redis.Get(r.Context(), ticketPrefix+util.Sha256Hex(ticket))
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"ok": false})
		return
	}
	if v == "" {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"ok": false})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ListSessions 列出本系统创建的 term-* tmux 会话。
func (s *Service) ListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := listTermSessions()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "读取终端会话失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// CloseSessions 批量关闭终端会话（names 不在白名单内的忽略）。
func (s *Service) CloseSessions(w http.ResponseWriter, r *http.Request) {
	body := httpx.Body(r)
	raw, _ := body["names"].([]any)
	killed := []string{}
	for i, v := range raw {
		if i >= 32 {
			break
		}
		name, _ := v.(string)
		if !sessionRe.MatchString(name) {
			continue
		}
		_ = killSession(name)
		killed = append(killed, name)
	}
	if len(killed) > 0 {
		util.Audit(s.cfg.AuditLog, "term_sessions_close", util.ClientIP(r), true, strings.Join(killed, ","))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "killed": killed})
}

// KillSession 关闭指定终端会话。
func (s *Service) KillSession(w http.ResponseWriter, r *http.Request) {
	name := httpx.Param(r, "name")
	if !sessionRe.MatchString(name) {
		httpx.Error(w, http.StatusBadRequest, "非法会话名")
		return
	}
	if err := killSession(name); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "关闭终端会话失败: "+err.Error())
		return
	}
	util.Audit(s.cfg.AuditLog, "term_session_kill", util.ClientIP(r), true, name)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// passwordOK 校验终端口令文件（格式 sha256$<salt>$<hash>，文件缺失/损坏一律拒绝）。
func (s *Service) passwordOK(pw string) bool {
	raw, err := os.ReadFile(s.cfg.TermPwFile)
	if err != nil {
		return false
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), "$")
	if len(parts) != 3 || parts[0] != "sha256" || parts[1] == "" || parts[2] == "" {
		return false
	}
	sum := sha256.Sum256([]byte(parts[1] + ":" + pw))
	want, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	return len(sum) == len(want) && subtle.ConstantTimeCompare(sum[:], want) == 1
}

type sessionInfo struct {
	Name     string `json:"name"`
	Attached bool   `json:"attached"`
	Activity int64  `json:"activity"`
}

func listTermSessions() ([]sessionInfo, error) {
	out, err := runShell("tmux list-sessions -F '#{session_name}|#{session_attached}|#{session_activity}' 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	sessions := []sessionInfo{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 3 {
			continue
		}
		if !sessionRe.MatchString(fields[0]) {
			continue
		}
		var activity int64
		fmt.Sscanf(fields[2], "%d", &activity)
		sessions = append(sessions, sessionInfo{Name: fields[0], Attached: fields[1] == "1", Activity: activity})
	}
	return sessions, nil
}

func killSession(name string) error {
	// name 已通过白名单正则校验，单引号包裹后再执行，等价旧实现。
	_, err := runShell("tmux kill-session -t '" + name + "' 2>/dev/null || true")
	return err
}

func runShell(command string) (string, error) {
	out, err := exec.Command("sh", "-c", command).Output()
	return string(out), err
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
