package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"admin-server-go/internal/httpx"
	"admin-server-go/internal/store"
	"admin-server-go/internal/util"
)

// okOnly 是 `{ ok: true }` 响应体。
type okOnly struct {
	Ok bool `json:"ok"`
}

// okCount 是 `{ ok: true, count: N }` 响应体。
type okCount struct {
	Ok    bool  `json:"ok"`
	Count int64 `json:"count"`
}

// detailResult 是 `{ item: ... }` 响应体。
type detailResult struct {
	Item NotificationItem `json:"item"`
}

// createResult 是写入通知的成功响应体（attachments 永远是非 nil 切片）。
type createResult struct {
	ID                  int64            `json:"id"`
	Ts                  int64            `json:"ts"`
	Attachments         []AttachmentView `json:"attachments"`
	AttachmentsWarnings []string         `json:"attachmentsWarnings,omitempty"`
}

// Create 处理 POST /api/admin/notifications：写入通知，可选 base64 附件落盘。
// body: { level?, type?, source, title, body?, link?, dedupKey?, attachments? } → 201 { id, ts, attachments }。
func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	ip := util.ClientIP(r)
	b := httpx.Body(r)
	levelInput := trimmedField(b, "level")
	source := trimmedField(b, "source")
	typeKey := trimmedField(b, "type")
	if typeKey == "" {
		typeKey = source // 缺省用 source 兼容既有写入方
	}
	title := trimmedField(b, "title")
	if levelInput != "" && !store.NotificationLevels[levelInput] {
		httpx.Error(w, http.StatusBadRequest, "level 必须是 urgent / normal / digest")
		return
	}
	if source == "" || title == "" {
		httpx.Error(w, http.StatusBadRequest, "source 与 title 不能为空")
		return
	}
	// 附件校验/解码先于写入：非法请求不应产生通知（也不应落盘）。
	parsed := parseAttachments(b["attachments"])
	if !parsed.ok {
		httpx.Error(w, parsed.status, parsed.err)
		return
	}
	res, err := s.writeNotification(r.Context(), writeInput{
		levelInput: levelInput,
		source:     source,
		typeKey:    typeKey,
		title:      title,
		body:       nonEmptyPtr(b, "body"),
		link:       nonEmptyPtr(b, "link"),
		dedupKey:   nonEmptyPtr(b, "dedupKey"),
	})
	if err != nil {
		util.Audit(s.cfg.AuditLog, "notification_write", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "写入通知失败")
		return
	}
	metas := []AttachmentMeta{}
	warnings := []string{}
	if len(parsed.items) > 0 {
		metas, warnings = s.saveNotificationAttachments(r.Context(), res.ID, parsed.items)
	}
	if len(warnings) > 0 {
		// 只记文件名与错误，不含 dataBase64/正文
		s.log.Warn("通知附件写入部分失败", "id", res.ID, "warnings", strings.Join(warnings, "; "))
	}
	// 重新取一次（带附件）再广播，保证 SSE 推送与列表/详情一致
	item := res.Item
	if fetched, ok, err := s.getNotification(r.Context(), res.ID); err == nil && ok {
		item = fetched
	}
	s.hub.broadcast(item)
	if res.Deduped {
		util.Audit(s.cfg.AuditLog, "notification_write", ip, true,
			fmt.Sprintf("dedup id=%d level=%s source=%s type=%s attachments=%d", res.ID, res.Level, source, typeKey, len(metas)))
	} else {
		util.Audit(s.cfg.AuditLog, "notification_write", ip, true,
			fmt.Sprintf("id=%d level=%s source=%s type=%s attachments=%d", res.ID, res.Level, source, typeKey, len(metas)))
	}
	payload := createResult{ID: res.ID, Ts: res.Ts, Attachments: toAttachmentViews(res.ID, metas)}
	if len(warnings) > 0 {
		payload.AttachmentsWarnings = warnings
	}
	httpx.WriteJSON(w, http.StatusCreated, payload)
}

// List 处理 GET /api/admin/notifications：支持 limit / before / level / source / type / unread。
func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	q := r.URL.Query()
	limit := int64(50)
	if v, ok := jsParseInt(q.Get("limit")); ok {
		limit = v
		if limit > 200 {
			limit = 200
		}
		if limit < 1 {
			limit = 1
		}
	}
	var before *int64
	if v, ok := jsParseInt(q.Get("before")); ok {
		before = &v
	}
	var level *string
	if lv := q.Get("level"); store.NotificationLevels[lv] {
		level = &lv
	}
	var source *string
	if v := strings.TrimSpace(q.Get("source")); v != "" {
		source = &v
	}
	var typ *string
	if v := strings.TrimSpace(q.Get("type")); v != "" {
		typ = &v
	}
	res, err := s.listNotifications(r.Context(), listFilters{
		limit:      limit,
		before:     before,
		level:      level,
		source:     source,
		typ:        typ,
		unreadOnly: q.Get("unread") == "1",
	})
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取通知失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// TypesList 处理 GET /api/admin/notifications/types：类别由服务端定义，客户端据此渲染筛选器。
func (s *Service) TypesList(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	res, err := s.listNotificationTypes(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取通知类别失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// TypesPatch 处理 PATCH /api/admin/notifications/types/:key：表即配置，只更新传入的字段。
func (s *Service) TypesPatch(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	key := strings.TrimSpace(httpx.Param(r, "key"))
	if key == "" {
		httpx.Error(w, http.StatusBadRequest, "无效的类别")
		return
	}
	b := httpx.Body(r)
	var patch typePatch
	if v, ok := jsonString(b, "label"); ok {
		label := strings.TrimSpace(v)
		if label == "" {
			httpx.Error(w, http.StatusBadRequest, "label 不能为空")
			return
		}
		patch.set("label", label)
	}
	if v, ok := jsonString(b, "description"); ok {
		patch.set("description", v)
	}
	if v, ok := jsonString(b, "defaultLevel"); ok {
		dl := strings.TrimSpace(v)
		if dl != "" && !store.NotificationLevels[dl] {
			httpx.Error(w, http.StatusBadRequest, "defaultLevel 必须是 urgent / normal / digest")
			return
		}
		if dl == "" {
			patch.set("default_level", nil)
		} else {
			patch.set("default_level", dl)
		}
	}
	if raw, ok := b["sort"]; ok && raw != nil {
		if str, isStr := raw.(string); !isStr || str != "" {
			n, valid := jsParseIntValue(raw)
			if !valid {
				httpx.Error(w, http.StatusBadRequest, "sort 必须是整数")
				return
			}
			patch.set("sort", n)
		}
	}
	if raw, ok := b["enabled"]; ok && raw != nil {
		patch.set("enabled", enabledValue(raw))
	}
	if patch.empty() {
		httpx.Error(w, http.StatusBadRequest, "没有可修改的字段")
		return
	}
	changes, err := s.updateNotificationType(r.Context(), key, patch)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "修改通知类别失败")
		return
	}
	if changes == 0 {
		httpx.Error(w, http.StatusNotFound, "类别不存在")
		return
	}
	util.Audit(s.cfg.AuditLog, "notification_type_update", util.ClientIP(r), true,
		fmt.Sprintf("key=%s fields=%d", key, patch.fields()))
	httpx.WriteJSON(w, http.StatusOK, okOnly{Ok: true})
}

// Read 处理 POST /api/admin/notifications/:id/read：单条置已读。
func (s *Service) Read(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	id, ok := jsParseInt(httpx.Param(r, "id"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "无效的 id")
		return
	}
	if err := s.markNotificationRead(r.Context(), id); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "标记已读失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "notification_read", util.ClientIP(r), true, fmt.Sprintf("id=%d", id))
	httpx.WriteJSON(w, http.StatusOK, okOnly{Ok: true})
}

// ReadAll 处理 POST /api/admin/notifications/read-all → { ok: true, count: N }。
func (s *Service) ReadAll(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	count, err := s.markAllNotificationsRead(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "全部已读失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "notification_read_all", util.ClientIP(r), true, fmt.Sprintf("count=%d", count))
	httpx.WriteJSON(w, http.StatusOK, okCount{Ok: true, Count: count})
}

// Delete 处理 DELETE /api/admin/notifications/:id：删除单条（连同附件）。
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	id, ok := jsParseInt(httpx.Param(r, "id"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "无效的 id")
		return
	}
	if err := s.deleteNotification(r.Context(), id); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "删除通知失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "notification_delete", util.ClientIP(r), true, fmt.Sprintf("id=%d", id))
	httpx.WriteJSON(w, http.StatusOK, okOnly{Ok: true})
}

// BulkDelete 处理 POST /api/admin/notifications/bulk-delete：按筛选一次删除多条。
// body: { level?, source?, unreadOnly?, readOnly?, dryRun? } → { ok: true, count: N }。
func (s *Service) BulkDelete(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	b := httpx.Body(r)
	var level *string
	if v, ok := jsonString(b, "level"); ok && store.NotificationLevels[v] {
		level = &v
	}
	var source *string
	if v := trimmedField(b, "source"); v != "" {
		source = &v
	}
	unreadOnly := boolField(b, "unreadOnly")
	readOnly := boolField(b, "readOnly")
	dryRun := boolField(b, "dryRun")
	// 未读与已读互斥：同时给出时不匹配任何行，避免误删
	if unreadOnly && readOnly {
		httpx.WriteJSON(w, http.StatusOK, okCount{Ok: true, Count: 0})
		return
	}
	count, err := s.bulkDeleteNotifications(r.Context(), bulkDeleteFilters{
		level:      level,
		source:     source,
		unreadOnly: unreadOnly,
		readOnly:   readOnly,
	}, dryRun)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "notification_bulk_delete", util.ClientIP(r), false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "批量删除失败")
		return
	}
	if !dryRun {
		util.Audit(s.cfg.AuditLog, "notification_bulk_delete", util.ClientIP(r), true,
			fmt.Sprintf("count=%d level=%s source=%s unreadOnly=%t readOnly=%t",
				count, dash(level), dash(source), unreadOnly, readOnly))
	}
	httpx.WriteJSON(w, http.StatusOK, okCount{Ok: true, Count: count})
}

// Stats 处理 GET /api/admin/notifications/stats：sources 按条数降序（同数按字典序）。
func (s *Service) Stats(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	res, err := s.notificationStats(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取通知统计失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// Stream 处理 GET /api/admin/notifications/stream：SSE 实时流（event: notification / heartbeat）。
func (s *Service) Stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		flusher = nopFlusher{}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if _, err := io.WriteString(w, "retry: 5000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	client := &sseClient{w: w, flusher: flusher}
	s.hub.add(client)
	ticker := time.NewTicker(time.Duration(store.NotificationHeartbeatMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			s.hub.remove(client)
			return
		case <-ticker.C:
			if !client.write(heartbeatFrame()) {
				s.hub.remove(client)
				return
			}
		}
	}
}

// Get 处理 GET /api/admin/notifications/:id：命中返回带附件的 item，不存在 404。
func (s *Service) Get(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	id, ok := jsParseInt(httpx.Param(r, "id"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "无效的 id")
		return
	}
	item, found, err := s.getNotification(r.Context(), id)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取通知详情失败")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "通知不存在")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detailResult{Item: item})
}

// Attachment 处理 GET /api/admin/notifications/:id/files/:name：附件下载。
// name 精确匹配落盘文件名；Content-Type 用入库 mime；text/html 额外下发 CSP sandbox + nosniff。
func (s *Service) Attachment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "通知库暂不可用")
		return
	}
	id, ok := jsParseInt(httpx.Param(r, "id"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "无效的 id")
		return
	}
	rawName := httpx.Param(r, "name")
	if rawName == "" {
		httpx.Error(w, http.StatusBadRequest, "无效的附件名")
		return
	}
	found, err := s.findAttachment(r.Context(), id, rawName)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "读取附件失败")
		return
	}
	if found == nil {
		httpx.Error(w, http.StatusNotFound, "附件不存在")
		return
	}
	f, err := os.Open(found.path)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "附件不存在")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", found.mime)
	w.Header().Set("Content-Disposition", "inline; filename=\""+found.name+"\"")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	for k, v := range attachmentDownloadHeaders(found.mime) {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// nopFlusher 用于不实现 http.Flusher 的 ResponseWriter（测试替身），保持 SSE 语义不崩。
type nopFlusher struct{}

func (nopFlusher) Flush() {}

// trimmedField 取请求体字符串字段并去首尾空白。
func trimmedField(body map[string]any, key string) string {
	v, _ := jsonString(body, key)
	return strings.TrimSpace(v)
}

// nonEmptyPtr 取字符串字段；非字符串或空串返回 nil（落库为 NULL）。
func nonEmptyPtr(body map[string]any, key string) *string {
	if v, ok := jsonString(body, key); ok && v != "" {
		return &v
	}
	return nil
}

// boolField 严格取布尔字段（对齐旧实现的 `=== true`）。
func boolField(body map[string]any, key string) bool {
	v, ok := body[key].(bool)
	return ok && v
}

// jsParseIntValue 把任意请求体值按 JS parseInt 语义转整数。
func jsParseIntValue(v any) (int64, bool) {
	switch n := v.(type) {
	case string:
		return jsParseInt(n)
	case float64:
		return jsParseInt(strconv.FormatFloat(n, 'f', -1, 64))
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		return jsParseInt(n.String())
	}
	return 0, false
}

// enabledValue 复刻旧实现的 `true | 1 | '1' ⇒ 1，否则 0`。
func enabledValue(v any) int64 {
	switch n := v.(type) {
	case bool:
		if n {
			return 1
		}
	case float64:
		if n == 1 {
			return 1
		}
	case string:
		if n == "1" {
			return 1
		}
	}
	return 0
}

// dash 用于审计详情里把空筛选渲染成 `-`。
func dash(p *string) string {
	if p == nil {
		return "-"
	}
	return *p
}
