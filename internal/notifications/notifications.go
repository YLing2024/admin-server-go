// Package notifications 复刻旧实现 src/notifications/*.ts 与 src/routes/notifications.ts：
// 通知表读写与去重、类别注册/默认级别、保留期清理、base64 附件落盘与安全下载、SSE 实时推送，
// 以及通知中心的全部 HTTP 处理器（鉴权由上游 httpapi 完成）。
//
// 字段名、状态码与中文错误串逐条对齐旧实现；通知库不可用时除 SSE 外的接口一律降级为 503。
package notifications

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/store"
)

// AttachmentMeta 是附件元信息（与文件本体分离，供列表/详情/下载回 mime）。
type AttachmentMeta struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
}

// AttachmentView 是附件的接口视图：元信息 + 同源下载地址。
type AttachmentView struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Size int    `json:"size"`
	URL  string `json:"url"`
}

// NotificationItem 是通知接口契约（字段名严格固定；dedup_key 不外泄）。
// attachments 永远是非 nil 切片，空数组序列化为 []，与旧实现的 [] 一致。
type NotificationItem struct {
	ID          int64            `json:"id"`
	Ts          int64            `json:"ts"`
	Level       string           `json:"level"`
	Source      string           `json:"source"`
	Type        string           `json:"type"`
	Title       string           `json:"title"`
	Body        *string          `json:"body"`
	Link        *string          `json:"link"`
	ReadAt      *int64           `json:"readAt"`
	Attachments []AttachmentView `json:"attachments"`
	HasImage    bool             `json:"hasImage"`
}

// NotificationType 是通知类别的接口视图（客户端据此动态渲染筛选器）。
type NotificationType struct {
	Key          string  `json:"key"`
	Label        string  `json:"label"`
	Description  *string `json:"description"`
	DefaultLevel *string `json:"defaultLevel"`
	Sort         int     `json:"sort"`
	Enabled      int     `json:"enabled"`
	Count        int     `json:"count"`
	Unread       int     `json:"unread"`
}

// Options 是 Service 的装配参数。
type Options struct {
	Cfg *config.Config
	// DB 为 nil 表示通知库不可用，除 SSE 外的接口按旧实现降级为 503。
	DB  *store.DB
	Log *slog.Logger
}

// Service 是通知中心域的服务。
type Service struct {
	cfg *config.Config
	db  *store.DB
	log *slog.Logger
	hub *sseHub
}

// New 构造通知服务；Log 为空时回落到 slog.Default。
func New(opts Options) *Service {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: opts.Cfg, db: opts.DB, log: log, hub: newSSEHub()}
}

// StartMaintenance 启动保留期清理循环（启动即清一次，其后每小时一次）。
// 删除过期通知时连同附件目录与元信息一并清理；失败只 warn，不影响其它接口。
func (s *Service) StartMaintenance() {
	s.cleanupExpired()
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.cleanupExpired()
		}
	}()
}

// notificationRow 是通知表原始行（可空列用 sql.Null* 承载）。
type notificationRow struct {
	ID     int64
	Ts     int64
	Level  string
	Source string
	Type   sql.NullString
	Title  string
	Body   sql.NullString
	Link   sql.NullString
	ReadAt sql.NullInt64
}

// view 把库内行映射为接口契约 item；type 历史缺失时回退 source。
func (s *Service) view(row notificationRow, metas []AttachmentMeta) NotificationItem {
	typ := row.Type.String
	if !row.Type.Valid || typ == "" {
		typ = row.Source
	}
	item := NotificationItem{
		ID:          row.ID,
		Ts:          row.Ts,
		Level:       row.Level,
		Source:      row.Source,
		Type:        typ,
		Title:       row.Title,
		Attachments: toAttachmentViews(row.ID, metas),
		HasImage:    hasImageAttachment(metas),
	}
	if row.Body.Valid {
		v := row.Body.String
		item.Body = &v
	}
	if row.Link.Valid {
		v := row.Link.String
		item.Link = &v
	}
	if row.ReadAt.Valid {
		v := row.ReadAt.Int64
		item.ReadAt = &v
	}
	return item
}

// nowSeconds 与旧实现 Date.now()/1000 取整一致，便于统一测试时钟。
func nowSeconds() int64 { return config.NowMillis() / 1000 }

// jsParseInt 复刻 JS parseInt：跳前导空白、可选正负号、取连续十进制数字；
// 无数字返回 false；溢出按 int64 边界收敛（旧实现会得到有限大数，调用方再 clamp）。
func jsParseInt(raw string) (int64, bool) {
	i := 0
	for i < len(raw) && isJSSpace(raw[i]) {
		i++
	}
	start := i
	if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
		i++
	}
	digits := i
	for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
		i++
	}
	if i == digits {
		return 0, false
	}
	v, err := strconv.ParseInt(raw[start:i], 10, 64)
	if err != nil {
		// 仅可能为 ErrRange：按符号收敛到边界，模拟 JS 的有限大数。
		if strings.HasPrefix(raw[start:], "-") {
			return -1 << 62, true
		}
		return 1 << 62, true
	}
	return v, true
}

func isJSSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// jsonString 读取请求体里的字符串字段（严格 typeof === 'string'，数组/数字不算）。
func jsonString(body map[string]any, key string) (string, bool) {
	s, ok := body[key].(string)
	return s, ok
}

// marshalNoEscape 输出不含 HTML 转义的 JSON（对齐 JS JSON.stringify 的 <> & 行为）。
func marshalNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(buf.String(), "\n")
}
