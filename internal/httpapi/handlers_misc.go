package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"admin-server-go/internal/alerts"
	"admin-server-go/internal/auth"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/logs"
	"admin-server-go/internal/ops"
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

// ---- 告警规则 ----

// handleAlertRules 列出全部规则。
func (s *Server) handleAlertRules(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	items, err := s.alerts.ListRules(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "读取告警规则失败")
		return
	}
	if items == nil {
		items = []alerts.Rule{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleAlertRuleCreate 新建规则。
func (s *Server) handleAlertRuleCreate(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		httpx.Error(w, http.StatusNotImplemented, "告警未启用")
		return
	}
	body := httpx.Body(r)
	rule := alerts.Rule{
		Name:        strings.TrimSpace(httpx.BodyString(body, "name")),
		Metric:      strings.TrimSpace(httpx.BodyString(body, "metric")),
		Op:          strings.TrimSpace(httpx.BodyString(body, "op")),
		Severity:    strings.TrimSpace(httpx.BodyString(body, "severity")),
		Threshold:   atofDefault(httpx.BodyString(body, "threshold"), 0),
		DurationSec: atoiDefault(httpx.BodyString(body, "durationSec"), 60),
		CooldownSec: atoiDefault(httpx.BodyString(body, "cooldownSec"), 900),
		Enabled:     true,
	}
	if rule.Name == "" || rule.Metric == "" || rule.Op == "" {
		httpx.Error(w, http.StatusBadRequest, "name / metric / op 必填")
		return
	}
	if rule.Severity == "" {
		rule.Severity = "warn"
	}
	created, err := s.alerts.CreateRule(r.Context(), rule)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "创建规则失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, created)
}

// handleAlertRuleUpdate 目前只支持启停。
func (s *Server) handleAlertRuleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		httpx.Error(w, http.StatusNotImplemented, "告警未启用")
		return
	}
	id, err := strconv.ParseInt(httpx.Param(r, "id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "id 非法")
		return
	}
	body := httpx.Body(r)
	enabled := httpx.BodyString(body, "enabled")
	on := enabled == "1" || strings.EqualFold(enabled, "true")
	if err := s.alerts.ToggleRule(r.Context(), id, on); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "更新规则失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAlertRuleDelete 删除规则。
func (s *Server) handleAlertRuleDelete(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		httpx.Error(w, http.StatusNotImplemented, "告警未启用")
		return
	}
	id, err := strconv.ParseInt(httpx.Param(r, "id"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "id 非法")
		return
	}
	if err := s.alerts.DeleteRule(r.Context(), id); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "删除规则失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAlertEvents 返回最近触发事件。
func (s *Server) handleAlertEvents(w http.ResponseWriter, r *http.Request) {
	if s.alerts == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	items, err := s.alerts.ListEvents(r.Context(), atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "读取告警事件失败")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// atofDefault 解析浮点，失败返回默认值。
func atofDefault(raw string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return def
	}
	return v
}

// handleOps 执行受控运维动作（服务启停 / 容器重启）。
// 安全约束在 ops.Service 内部：confirm 必须等于目标名、服务有拒绝名单、3 秒限速、30 秒超时。
func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	if s.ops == nil {
		httpx.Error(w, http.StatusNotImplemented, "运维动作未启用")
		return
	}
	body := httpx.Body(r)
	req := ops.Request{
		Kind:    strings.TrimSpace(httpx.BodyString(body, "kind")),
		Name:    strings.TrimSpace(httpx.BodyString(body, "name")),
		Action:  strings.TrimSpace(httpx.BodyString(body, "action")),
		Confirm: strings.TrimSpace(httpx.BodyString(body, "confirm")),
	}
	res, err := s.ops.Execute(r.Context(), req)
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error(), "result": res})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
