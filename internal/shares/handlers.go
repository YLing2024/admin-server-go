package shares

import (
	"crypto/rand"
	"encoding/base64"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

// Create 处理 POST /api/admin/files/shares：body {path, expiresAt? | ttlHours?, note?}。
func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	body := httpx.Body(r)
	full, ok := s.resolveSharePath(body["path"])
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	if s.files.IsProtected(full) {
		httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许分享")
		return
	}
	info := shareFileInfo(full, true)
	switch info.kind {
	case "missing":
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	case "dir":
		httpx.Error(w, http.StatusBadRequest, "暂不支持分享目录")
		return
	case "file":
	default:
		httpx.Error(w, http.StatusBadRequest, "不是普通文件")
		return
	}

	now := config.NowMillis()
	expiresAt, msg := s.resolveCreateExpiry(body, now)
	if msg != "" {
		httpx.Error(w, http.StatusBadRequest, msg)
		return
	}

	rec := shareRecord{
		ID:           randomBase64URL(6),
		Token:        randomBase64URL(24),
		RelPath:      s.files.RelFromFull(full),
		Size:         info.size,
		Note:         readNote(body),
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		RevokedAt:    nil,
		Downloads:    0,
		LastAccessAt: nil,
		LastAccessIP: nil,
	}

	s.mu.Lock()
	list := s.readShares()
	list = append(list, rec)
	s.writeShares(list)
	s.mu.Unlock()

	util.Audit(s.cfg.AuditLog, "share-create", ip, true,
		"id="+rec.ID+" path="+rec.RelPath+" expiresAt="+strconv.FormatInt(rec.ExpiresAt, 10))
	httpx.WriteJSON(w, http.StatusCreated, s.toShareView(rec, r, now))
}

// List 处理 GET /api/admin/files/shares：按 createdAt 倒序返回全部分享视图。
func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	now := config.NowMillis()
	s.mu.Lock()
	list := s.readShares()
	s.mu.Unlock()
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	views := make([]shareView, 0, len(list))
	for _, rec := range list {
		views = append(views, s.toShareView(rec, r, now))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"shares": views})
}

// Update 处理 PATCH /api/admin/files/shares/:id：body {expiresAt?, note?, revoked?}。
func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id := httpx.Param(r, "id")
	body := httpx.Body(r)
	now := config.NowMillis()

	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.readShares()
	idx := findIndex(list, id)
	if idx == -1 {
		httpx.Error(w, http.StatusNotFound, "链接不存在")
		return
	}
	rec := list[idx]

	wantsRevoke := false
	if revokedVal, present := body["revoked"]; present {
		b, isBool := revokedVal.(bool)
		if !isBool {
			httpx.Error(w, http.StatusBadRequest, "revoked 须为布尔值")
			return
		}
		if !b {
			httpx.Error(w, http.StatusBadRequest, "撤销不可逆，无法恢复")
			return
		}
		wantsRevoke = true
	}
	hasExpiry := keyPresent(body, "expiresAt")
	_, hasNote := body["note"]
	if !wantsRevoke && !hasExpiry && !hasNote {
		httpx.Error(w, http.StatusBadRequest, "没有可更新的字段")
		return
	}

	// 状态机：仅 active 可改期 / 撤销（expired / revoked 不可再操作）
	status := s.shareStatus(rec, now)
	if status != "active" {
		httpx.Error(w, http.StatusBadRequest, "仅有效链接可操作（当前 "+status+"）")
		return
	}

	if wantsRevoke {
		revoked := now
		rec.RevokedAt = &revoked
	}
	if hasExpiry {
		at, msg := validateExpiry(body["expiresAt"], now)
		if msg != "" {
			httpx.Error(w, http.StatusBadRequest, msg)
			return
		}
		rec.ExpiresAt = at
	}
	if hasNote {
		rec.Note = readNote(body)
	}

	list[idx] = rec
	s.writeShares(list)
	revokedFlag := 0
	if wantsRevoke {
		revokedFlag = 1
	}
	util.Audit(s.cfg.AuditLog, "share-update", ip, true,
		"id="+id+" revoked="+strconv.Itoa(revokedFlag)+" expiresAt="+strconv.FormatInt(rec.ExpiresAt, 10))
	httpx.WriteJSON(w, http.StatusOK, s.toShareView(rec, r, now))
}

// Delete 处理 DELETE /api/admin/files/shares/:id：只删账本记录，不动磁盘文件。
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id := httpx.Param(r, "id")

	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.readShares()
	idx := findIndex(list, id)
	if idx == -1 {
		httpx.Error(w, http.StatusNotFound, "链接不存在")
		return
	}
	removed := list[idx]
	list = append(list[:idx], list[idx+1:]...)
	s.writeShares(list)
	util.Audit(s.cfg.AuditLog, "share-delete", ip, true, "id="+id+" path="+removed.RelPath)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// PublicDownload 处理免鉴权的 GET /s/:token：只认 token，不接受路径参数。
// active 命中时计数并记录最后访问；revoked/expired 返回 410，缺失或被删返回 404。
func (s *Service) PublicDownload(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	now := config.NowMillis()
	token := httpx.Param(r, "token")
	if token == "" {
		shareFail(w, http.StatusNotFound, "链接不存在")
		return
	}

	s.mu.Lock()
	list := s.readShares()
	idx := findIndexByToken(list, token)
	if idx == -1 {
		s.mu.Unlock()
		shareFail(w, http.StatusNotFound, "链接不存在")
		return
	}
	rec := list[idx]
	status := s.shareStatus(rec, now)
	if status == "revoked" || status == "expired" {
		s.mu.Unlock()
		shareFail(w, http.StatusGone, "链接已失效")
		return
	}
	full, ok := s.shareFullFromRel(rec.RelPath)
	if !ok || shareFileInfo(full, ok).kind != "file" {
		s.mu.Unlock()
		shareFail(w, http.StatusNotFound, "链接不存在")
		return
	}
	// 成功下载：计数 + 记录最后访问，并写审计（先落账再发送，语义对齐旧实现）
	rec.Downloads++
	accessAt := now
	accessIP := ip
	rec.LastAccessAt = &accessAt
	rec.LastAccessIP = &accessIP
	list[idx] = rec
	s.writeShares(list)
	s.mu.Unlock()

	util.Audit(s.cfg.AuditLog, "share-download", ip, true, rec.RelPath)
	s.files.ServeAttachment(w, r, full, filepath.Base(full))
}

// resolveCreateExpiry 解析创建时的有效期：expiresAt 优先于 ttlHours，都不给默认 24 小时。
// 返回空字符串 msg 表示成功。
func (s *Service) resolveCreateExpiry(body map[string]any, now int64) (int64, string) {
	if keyPresent(body, "expiresAt") {
		return validateExpiry(body["expiresAt"], now)
	}
	if keyPresent(body, "ttlHours") {
		h, ok := jsNumber(body["ttlHours"])
		if !ok {
			return 0, "ttlHours 须为数字"
		}
		clamped := math.Min(shareMaxTTLHours, math.Max(shareMinTTLHours, h))
		return now + int64(clamped*3600*1000), ""
	}
	return now + shareDefaultTTLHours*3600*1000, ""
}

// validateExpiry 校验绝对到期时刻：须为 epoch 毫秒整数，0 为永久，非 0 须为将来时刻。
func validateExpiry(v any, now int64) (int64, string) {
	at, ok := jsNumber(v)
	if !ok || at != math.Trunc(at) || at < 0 {
		return 0, "expiresAt 须为 epoch 毫秒整数（0 表示永久）"
	}
	// JS 的 Number 可以远超 int64；超出范围时夹到 int64 上界，保持「远期有效」语义且避免溢出。
	if at > math.MaxInt64 {
		return math.MaxInt64, ""
	}
	iv := int64(at)
	if iv != sharePermanent && iv <= now {
		return 0, "expiresAt 须为将来时刻（0 表示永久；立即失效请用撤销）"
	}
	return iv, ""
}

// keyPresent 判断 body 中某键是否「已提供且非 null 非空串」，对齐旧实现的 hasAbs/hasExpiry 判定。
func keyPresent(body map[string]any, key string) bool {
	v, present := body[key]
	if !present || v == nil {
		return false
	}
	if str, ok := v.(string); ok && str == "" {
		return false
	}
	return true
}

// readNote 读取备注：仅字符串采用（trim + 截断 500 字），否则空串。
func readNote(body map[string]any) string {
	str, ok := body["note"].(string)
	if !ok {
		return ""
	}
	runes := []rune(strings.TrimSpace(str))
	if len(runes) > noteMaxRunes {
		runes = runes[:noteMaxRunes]
	}
	return string(runes)
}

// shareFail 输出极简 HTML 提示页（不新增文案、不回显服务器路径）。
func shareFail(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(shareFailPage(text)))
}

// shareFailPage 与旧实现逐字一致的中文提示页。
func shareFailPage(text string) string {
	return "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + text + "</title></head>" +
		"<body style=\"margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;" +
		"font-family:system-ui,-apple-system,'Segoe UI',sans-serif;background:#0f1115;color:#c9d1d9\">" +
		"<main style=\"text-align:center\"><p style=\"font-size:15px;letter-spacing:.05em\">" + text + "</p></main>" +
		"</body></html>"
}

// findIndex 按 id 查找记录下标，未命中返回 -1。
func findIndex(list []shareRecord, id string) int {
	for i, rec := range list {
		if rec.ID == id {
			return i
		}
	}
	return -1
}

// findIndexByToken 按 token 查找记录下标，未命中返回 -1。
func findIndexByToken(list []shareRecord, token string) int {
	for i, rec := range list {
		if rec.Token == token {
			return i
		}
	}
	return -1
}

// randomBase64URL 返回 n 字节随机数的 base64url 文本（与旧实现 randomBytes(n).toString('base64url') 等价）。
func randomBase64URL(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(config.NowMillis(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
