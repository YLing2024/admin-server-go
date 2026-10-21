package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"admin-server-go/internal/auth"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/logs"
	"admin-server-go/internal/util"
)

// ---- 接口令牌（/api/admin/api-tokens）----

// apiTokenView 是接口令牌的对外视图。字段顺序与旧实现 res.json 的键顺序一致，
// 便于与旧实例做响应快照对照；绝不包含令牌明文（明文仅在创建时返回一次）。
type apiTokenView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Note       string `json:"note"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
	CanWrite   bool   `json:"canWrite"`
}

func viewOfAPIToken(m *auth.APITokenMeta) apiTokenView {
	return apiTokenView{
		ID:         m.ID,
		Name:       m.Name,
		Note:       m.Note,
		CreatedAt:  m.CreatedAt,
		ExpiresAt:  m.ExpiresAt,
		LastUsedAt: m.LastUsedAt,
		CanWrite:   m.CanWrite,
	}
}

// handleAPITokensList 列出全部接口令牌（只含元数据），按 createdAt 倒序。
func (s *Server) handleAPITokensList(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	keys, err := s.redis.Keys(r.Context(), auth.APITokenPrefix+"*")
	if err != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_list", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "获取接口令牌失败")
		return
	}
	views := make([]apiTokenView, 0, len(keys))
	for _, key := range keys {
		raw, gerr := s.redis.Get(r.Context(), key)
		if gerr != nil {
			util.Audit(s.cfg.AuditLog, "api_tokens_list", ip, false, gerr.Error())
			httpx.Error(w, http.StatusInternalServerError, "获取接口令牌失败")
			return
		}
		meta := auth.ParseAPITokenMeta(raw)
		if meta == nil {
			continue
		}
		views = append(views, viewOfAPIToken(meta))
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].CreatedAt > views[j].CreatedAt })
	util.Audit(s.cfg.AuditLog, "api_tokens_list", ip, true, "count="+strconv.Itoa(len(views)))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tokens": views})
}

// handleAPITokensCreate 生成接口令牌；明文（64 位 hex）仅此一次返回。
func (s *Server) handleAPITokensCreate(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	body := httpx.Body(r)
	name := strings.TrimSpace(httpx.BodyString(body, "name"))
	note := strings.TrimSpace(httpx.BodyString(body, "note"))

	days := 30
	if raw, ok := body["expiresInDays"]; ok && raw != nil {
		n, ok := jsNumber(raw)
		if !ok || n != float64(int64(n)) {
			httpx.Error(w, http.StatusBadRequest, "有效期需为 1~365 天的整数")
			return
		}
		days = int(n)
	}
	if name == "" {
		httpx.Error(w, http.StatusBadRequest, "令牌名称不能为空")
		return
	}
	if days < 1 || days > auth.APITokenMaxDays {
		httpx.Error(w, http.StatusBadRequest, "有效期需为 1~365 天的整数")
		return
	}

	token, err := randomHex(32)
	if err != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_create", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "生成接口令牌失败")
		return
	}
	id := util.Sha256Hex(token)
	now := time.Now().UnixMilli()
	canWrite, _ := httpx.BodyBool(body, "canWrite") // 严格 === true
	meta := auth.APITokenMeta{
		ID:         id,
		Name:       name,
		Note:       note,
		CreatedAt:  now,
		ExpiresAt:  now + int64(days)*86400000,
		LastUsedAt: 0,
		CanWrite:   canWrite,
	}
	payload, merr := json.Marshal(meta)
	if merr != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_create", ip, false, merr.Error())
		httpx.Error(w, http.StatusInternalServerError, "生成接口令牌失败")
		return
	}
	if serr := s.redis.Set(r.Context(), auth.APITokenKey(id), string(payload), days*86400); serr != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_create", ip, false, serr.Error())
		httpx.Error(w, http.StatusInternalServerError, "生成接口令牌失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "api_tokens_create", ip, true,
		"id="+id+" name="+name+" days="+strconv.Itoa(days)+" canWrite="+strconv.FormatBool(canWrite))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "token": token, "meta": viewOfAPIToken(&meta)})
}

// handleAPITokensUpdate 改名 / 改备注 / 切换可写 / 重设有效期（重设则刷新 TTL）。
func (s *Server) handleAPITokensUpdate(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id := httpx.Param(r, "id")
	raw, err := s.redis.Get(r.Context(), auth.APITokenKey(id))
	if err != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_update", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "更新接口令牌失败")
		return
	}
	meta := auth.ParseAPITokenMeta(raw)
	if meta == nil {
		httpx.Error(w, http.StatusNotFound, "接口令牌不存在或已过期")
		return
	}
	body := httpx.Body(r)
	if v, ok := body["name"]; ok && v != nil {
		name := strings.TrimSpace(httpx.BodyString(body, "name"))
		if name == "" {
			httpx.Error(w, http.StatusBadRequest, "令牌名称不能为空")
			return
		}
		meta.Name = name
	}
	if v, ok := body["note"]; ok && v != nil {
		meta.Note = strings.TrimSpace(httpx.BodyString(body, "note"))
	}
	if _, ok := body["canWrite"]; ok {
		cw, _ := httpx.BodyBool(body, "canWrite")
		meta.CanWrite = cw
	}
	if raw, ok := body["expiresInDays"]; ok && raw != nil {
		n, ok := jsNumber(raw)
		if !ok || n != float64(int64(n)) {
			httpx.Error(w, http.StatusBadRequest, "有效期需为 1~365 天的整数")
			return
		}
		days := int(n)
		if days < 1 || days > auth.APITokenMaxDays {
			httpx.Error(w, http.StatusBadRequest, "有效期需为 1~365 天的整数")
			return
		}
		meta.ExpiresAt = time.Now().UnixMilli() + int64(days)*86400000
	}
	ttl := int((meta.ExpiresAt - time.Now().UnixMilli()) / 1000)
	if ttl < 1 {
		ttl = 1
	}
	payload, merr := json.Marshal(meta)
	if merr != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_update", ip, false, merr.Error())
		httpx.Error(w, http.StatusInternalServerError, "更新接口令牌失败")
		return
	}
	if serr := s.redis.Set(r.Context(), auth.APITokenKey(id), string(payload), ttl); serr != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_update", ip, false, serr.Error())
		httpx.Error(w, http.StatusInternalServerError, "更新接口令牌失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "api_tokens_update", ip, true, "id="+id)
	httpx.WriteJSON(w, http.StatusOK, viewOfAPIToken(meta))
}

// handleAPITokensDelete 吊销令牌，立即失效（幂等）。
func (s *Server) handleAPITokensDelete(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id := httpx.Param(r, "id")
	if err := s.redis.Del(r.Context(), auth.APITokenKey(id)); err != nil {
		util.Audit(s.cfg.AuditLog, "api_tokens_delete", ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, "吊销接口令牌失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "api_tokens_delete", ip, true, "id="+id)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// jsNumber 近似 JS Number()：支持 JSON 数字、表单字符串与布尔。
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// handleLogs 处理 GET /api/admin/logs：从云 MySQL 的日志库只读检索。
// 查询参数：source=system|container|file|access|error, q=关键字, since=ISO 或 24h/7d, limit, offset。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logs == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "日志库未配置")
		return
	}
	q := r.URL.Query()
	src := logs.Source(strings.TrimSpace(q.Get("source")))
	if src == "" {
		src = logs.SourceSystem
	}
	query := logs.Query{
		Source: src,
		Q:      q.Get("q"),
		Limit:  atoiDefault(q.Get("limit"), 200),
		Offset: atoiDefault(q.Get("offset"), 0),
	}
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		if d, err := parseSince(raw); err == nil {
			query.Since = time.Now().Add(-d)
		}
	}
	res, err := s.logs.Query(r.Context(), query)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "日志查询失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// parseSince 支持 "30m" / "24h" / "7d" 这类相对时长。
func parseSince(raw string) (time.Duration, error) {
	if len(raw) < 2 {
		return 0, fmt.Errorf("bad duration %q", raw)
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad duration %q", raw)
	}
	switch raw[len(raw)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("bad unit %q", raw)
}

// atoiDefault 解析整数，失败或非正数时返回默认值。
func atoiDefault(raw string, def int) int {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// handleAnomalies 返回最近的基线异常事件（谁偏离了自己的历史）。
func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.anomaly == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	items, err := s.anomaly.List(r.Context(), atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "异常查询失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
