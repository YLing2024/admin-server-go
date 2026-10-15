// Package history 实现 Hermes 历史会话的只读浏览：数据源 HERMES_STATE_DB，
// 每次请求独立「打开 → 查询 → 关闭」，只读打开，绝不长连接持有（Hermes 网关在写同一个库）。
// 语义逐条对齐旧实现 src/routes/history.ts。
package history

import (
	"database/sql"
	"log/slog"
	"math"
	"net/http"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"

	// 注册 modernc sqlite 驱动（只读打开 state.db）。
	_ "modernc.org/sqlite"
)

// Options 是装配参数。
type Options struct {
	Cfg *config.Config
	Log *slog.Logger
}

// Service 提供历史浏览 handler。
type Service struct {
	cfg *config.Config
	log *slog.Logger
}

// New 构造历史服务。
func New(opts Options) *Service { return &Service{cfg: opts.Cfg, log: opts.Log} }

func (s *Service) open() (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+s.cfg.HermesStateDB+"?mode=ro&_pragma=busy_timeout(3000)")
}

// List 返回会话列表（按最后活动时间倒序）。
func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	db, err := s.open()
	if err != nil {
		util.Audit(s.cfg.AuditLog, "history_list", ip, false, "数据库不可用: "+err.Error())
		httpx.Error(w, http.StatusServiceUnavailable, "历史记录暂不可用")
		return
	}
	defer db.Close()

	rows, err := db.QueryContext(r.Context(),
		`SELECT id, title, display_name, started_at, last_activity_at, message_count, session_key
		 FROM sessions ORDER BY last_activity_at DESC`)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "history_list", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取历史会话失败")
		return
	}
	defer rows.Close()

	sessions := []map[string]any{}
	for rows.Next() {
		var (
			id, sessionKey          sql.NullString
			title, displayName      sql.NullString
			startedAt, lastActivity sql.NullFloat64
			messageCount            sql.NullInt64
		)
		if err := rows.Scan(&id, &title, &displayName, &startedAt, &lastActivity, &messageCount, &sessionKey); err != nil {
			util.Audit(s.cfg.AuditLog, "history_list", ip, false, err.Error())
			httpx.Error(w, http.StatusInternalServerError, "获取历史会话失败")
			return
		}
		titleText := firstNonEmpty(title.String, displayName.String, id.String)
		var timeVal any
		raw := lastActivity
		if !raw.Valid {
			raw = startedAt
		}
		if raw.Valid {
			timeVal = int64(math.Round(raw.Float64 * 1000))
		}
		sessions = append(sessions, map[string]any{
			"id":            id.String,
			"title":         titleText,
			"time":          timeVal,
			"message_count": messageCount.Int64,
		})
	}
	if err := rows.Err(); err != nil {
		util.Audit(s.cfg.AuditLog, "history_list", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取历史会话失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "history_list", ip, true, "sessions="+itoa(len(sessions)))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// Detail 返回单个会话的 user/assistant 消息（按 id 升序）。
func (s *Service) Detail(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id := httpx.Param(r, "id")
	db, err := s.open()
	if err != nil {
		util.Audit(s.cfg.AuditLog, "history_messages", ip, false, "数据库不可用: "+err.Error())
		httpx.Error(w, http.StatusServiceUnavailable, "历史记录暂不可用")
		return
	}
	defer db.Close()

	var (
		sessionID, title, displayName sql.NullString
	)
	err = db.QueryRowContext(r.Context(), `SELECT id, title, display_name FROM sessions WHERE id = ?`, id).
		Scan(&sessionID, &title, &displayName)
	if err == sql.ErrNoRows {
		util.Audit(s.cfg.AuditLog, "history_messages", ip, false, "会话不存在: "+id)
		httpx.Error(w, http.StatusNotFound, "会话不存在")
		return
	}
	if err != nil {
		util.Audit(s.cfg.AuditLog, "history_messages", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取会话消息失败")
		return
	}

	rows, err := db.QueryContext(r.Context(),
		`SELECT role, content, timestamp FROM messages
		 WHERE session_id = ? AND role IN ('user','assistant') ORDER BY id ASC`, id)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "history_messages", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取会话消息失败")
		return
	}
	defer rows.Close()

	messages := []map[string]any{}
	for rows.Next() {
		var (
			role      sql.NullString
			content   sql.NullString
			timestamp sql.NullFloat64
		)
		if err := rows.Scan(&role, &content, &timestamp); err != nil {
			util.Audit(s.cfg.AuditLog, "history_messages", ip, false, err.Error())
			httpx.Error(w, http.StatusInternalServerError, "获取会话消息失败")
			return
		}
		var ts any
		if timestamp.Valid {
			ts = int64(math.Round(timestamp.Float64 * 1000))
		}
		messages = append(messages, map[string]any{"role": role.String, "content": content.String, "ts": ts})
	}
	if err := rows.Err(); err != nil {
		util.Audit(s.cfg.AuditLog, "history_messages", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取会话消息失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "history_messages", ip, true, "session="+id+" messages="+itoa(len(messages)))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"session":  map[string]any{"id": sessionID.String, "title": firstNonEmpty(title.String, displayName.String, sessionID.String)},
		"messages": messages,
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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
