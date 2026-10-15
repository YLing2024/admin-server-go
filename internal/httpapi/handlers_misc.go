package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/auth"
	"admin-server-go/internal/httpx"
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

// ---- 软件版本（/api/admin/versions）----

// versionCheck 是一条版本采集指令；名称/分类/命令与旧实现逐字一致。
type versionCheck struct {
	Name     string
	Category string
	Cmd      string
}

// nodeBin 与旧实现一致：本机 Node 运行时路径。
const nodeBin = "/root/.nvm/versions/node/v24.19.0/bin"

// versionChecks 是全部版本采集项（顺序即返回顺序）。
var versionChecks = []versionCheck{
	{Name: "Node.js", Category: "runtime", Cmd: nodeBin + "/node --version"},
	{Name: "npm", Category: "runtime", Cmd: "PATH=" + nodeBin + ":$PATH " + nodeBin + "/npm --version"},
	{Name: "Python", Category: "runtime", Cmd: "python3 --version 2>&1 | awk '{print $2}'"},
	{Name: "Hermes", Category: "service", Cmd: "/usr/local/bin/hermes version 2>/dev/null | head -1 | grep -oE 'v[0-9.]+' | head -1 | sed 's/^v//'"},
	{Name: "pi", Category: "service", Cmd: "cat " + nodeBin + "/lib/node_modules/@earendil-works/pi-coding-agent/package.json | grep -m1 '\"version\"' | grep -oE '[0-9]+\\.[0-9]+\\.[0-9]+'"},
	{Name: "Codex", Category: "service", Cmd: "cat " + nodeBin + "/lib/node_modules/@openai/codex/package.json | grep -m1 '\"version\"' | grep -oE '[0-9]+\\.[0-9]+\\.[0-9]+'"},
	{Name: "Playwright", Category: "service", Cmd: "PATH=" + nodeBin + ":$PATH " + nodeBin + "/playwright --version | grep -oE '[0-9]+\\.[0-9]+\\.[0-9]+' | head -1"},
	{Name: "dida-cli", Category: "service", Cmd: "cat " + nodeBin + "/lib/node_modules/@suibiji/dida-cli/package.json | grep -m1 \"version\" | grep -oE '[0-9]+\\.[0-9]+\\.[0-9]+'"},
	{Name: "DeepTutor", Category: "service", Cmd: "/root/proj/deeptutor/.venv/bin/pip show deeptutor | grep -m1 Version | awk '{print $2}'"},
}

// versionItem 是单条版本结果（JSON 键顺序与旧实现一致）。
type versionItem struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Version  string `json:"version"`
	OK       bool   `json:"ok"`
}

// versionCacheTTL 与旧实现一致：60 秒。
const versionCacheTTL = 60 * time.Second

// versionCache 缓存上一次采集结果，避免 1 秒轮询反复执行重命令。
var versionCache struct {
	mu   sync.Mutex
	ts   time.Time
	list []versionItem
}

// handleVersions 返回本地已安装软件版本（60 秒缓存）。
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	list, err := cachedVersions(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取软件版本失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"list": list})
}

// cachedVersions 命中 60 秒窗口则复用缓存，否则并行采集全部条目。
func cachedVersions(ctx context.Context) ([]versionItem, error) {
	versionCache.mu.Lock()
	defer versionCache.mu.Unlock()
	if versionCache.list != nil && time.Since(versionCache.ts) <= versionCacheTTL {
		return versionCache.list, nil
	}
	list := collectVersions(ctx)
	versionCache.list = list
	versionCache.ts = time.Now()
	return list, nil
}

// collectVersions 并行执行全部版本命令，单条失败只标记该条 ok=false，不影响其余。
func collectVersions(ctx context.Context) []versionItem {
	items := make([]versionItem, len(versionChecks))
	var wg sync.WaitGroup
	for i, vc := range versionChecks {
		wg.Add(1)
		go func(i int, vc versionCheck) {
			defer wg.Done()
			items[i] = runVersionCheck(ctx, vc)
		}(i, vc)
	}
	wg.Wait()
	return items
}

// runVersionCheck 执行单条命令（8 秒超时，shell 语义），取首行过 stripVersion。
func runVersionCheck(ctx context.Context, vc versionCheck) versionItem {
	item := versionItem{Name: vc.Name, Category: vc.Category, Version: "未知", OK: false}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "sh", "-c", vc.Cmd).Output()
	if err != nil {
		return item
	}
	line := strings.TrimSpace(string(out))
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	item.Version = stripVersion(line)
	if item.Version == "" {
		item.Version = "未知"
	}
	item.OK = true
	return item
}

// stripVersion 去 v/V 前缀、去开头的 epoch（数字+冒号）、取第一个 '-' 前的主版本段。
// 例：'5:8.0.2-3+deb13u2' → '8.0.2'；'v24.19.0' → '24.19.0'。
func stripVersion(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	if c := s[0]; c == 'v' || c == 'V' {
		s = s[1:]
	}
	s = trimEpochPrefix(s)
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// trimEpochPrefix 去掉形如 '5:' 的 Debian epoch 前缀（仅当冒号前全为数字）。
func trimEpochPrefix(s string) string {
	idx := strings.IndexByte(s, ':')
	if idx <= 0 {
		return s
	}
	for i := 0; i < idx; i++ {
		if s[i] < '0' || s[i] > '9' {
			return s
		}
	}
	return s[idx+1:]
}
