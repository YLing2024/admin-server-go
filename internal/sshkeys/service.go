package sshkeys

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/store"
	"admin-server-go/internal/util"
)

// Options 是公钥管理服务的装配参数。
type Options struct {
	Cfg *config.Config
	DB  *store.DB
	Log *slog.Logger
}

// Service 提供四个管理端接口与辅助命令共用的仓储。
type Service struct {
	cfg  *config.Config
	log  *slog.Logger
	repo *Repository
}

// New 构造服务。DB 为 nil 时接口返回 503，但不影响进程其他功能。
func New(opts Options) *Service {
	return &Service{cfg: opts.Cfg, log: opts.Log, repo: NewRepository(opts.DB)}
}

// Repository 暴露仓储给独立辅助命令复用。
func (s *Service) Repository() *Repository { return s.repo }

// keyView 是对外视图。id 用字符串：BIGINT 超出 JS 安全整数范围后转 Number 会丢精度。
type keyView struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"keyType"`
	Comment     string `json:"comment"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"createdAt"`
	LastUsedAt  *int64 `json:"lastUsedAt"`
}

func viewOf(key Key) keyView {
	return keyView{
		ID:          strconv.FormatInt(key.ID, 10),
		Title:       key.Title,
		Fingerprint: fingerprintDisplay(key.Fingerprint),
		KeyType:     key.KeyType,
		Comment:     key.Comment,
		Enabled:     key.Enabled,
		CreatedAt:   key.CreatedAt,
		LastUsedAt:  key.LastUsedAt,
	}
}

// fingerprintDisplay 把库里的 hex 指纹还原成 GitHub 风格展示值。
func fingerprintDisplay(hexValue string) string {
	parsed, err := fingerprintFromHex(hexValue)
	if err != nil {
		return hexValue
	}
	return parsed
}

// List 处理 GET /api/admin/ssh-keys。
func (s *Service) List(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	keys, err := s.repo.List(r.Context())
	if err != nil {
		s.fail(w, ip, "ssh_keys_list", err, "获取 SSH 公钥失败")
		return
	}
	views := make([]keyView, 0, len(keys))
	for _, key := range keys {
		views = append(views, viewOf(key))
	}
	util.Audit(s.cfg.AuditLog, "ssh_keys_list", ip, true, "count="+strconv.Itoa(len(views)))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": views})
}

// Create 处理 POST /api/admin/ssh-keys：服务端解析公钥、去重、限量后再入库。
func (s *Service) Create(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	body := httpx.Body(r)

	title, err := ValidateTitle(httpx.BodyString(body, "title"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := ParsePublicKey(httpx.BodyString(body, "public_key"))
	if err != nil {
		util.Audit(s.cfg.AuditLog, "ssh_keys_create", ip, false, "公钥解析失败")
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	key, err := s.repo.Create(r.Context(), title, parsed)
	if err != nil {
		switch {
		case errors.Is(err, ErrDuplicate):
			util.Audit(s.cfg.AuditLog, "ssh_keys_create", ip, false, "指纹重复")
			httpx.Error(w, http.StatusConflict, "该公钥已存在")
		case errors.Is(err, ErrLimit):
			util.Audit(s.cfg.AuditLog, "ssh_keys_create", ip, false, "已达上限")
			httpx.Error(w, http.StatusBadRequest, "最多只能添加 20 把公钥")
		default:
			s.fail(w, ip, "ssh_keys_create", err, "添加 SSH 公钥失败")
		}
		return
	}
	util.Audit(s.cfg.AuditLog, "ssh_keys_create", ip, true,
		"id="+strconv.FormatInt(key.ID, 10)+" fingerprint="+parsed.Fingerprint)
	httpx.WriteJSON(w, http.StatusCreated, viewOf(key))
}

// Update 处理 PATCH /api/admin/ssh-keys/:id：只允许改 title 与 enabled。
func (s *Service) Update(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id, err := keyID(r)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	current, err := s.repo.Get(r.Context(), id)
	if err != nil {
		s.fail(w, ip, "ssh_keys_update", err, "更新 SSH 公钥失败")
		return
	}

	body := httpx.Body(r)
	title := current.Title
	enabled := current.Enabled
	changed := false

	if raw, ok := body["title"]; ok && raw != nil {
		value, ok := raw.(string)
		if !ok {
			httpx.Error(w, http.StatusBadRequest, "标题必须为字符串")
			return
		}
		title, err = ValidateTitle(value)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		changed = true
	}
	if raw, ok := body["enabled"]; ok {
		value, ok := raw.(bool)
		if !ok {
			httpx.Error(w, http.StatusBadRequest, "enabled 必须为布尔值")
			return
		}
		enabled = value
		changed = true
	}
	if !changed {
		httpx.Error(w, http.StatusBadRequest, "没有可更新的字段")
		return
	}

	key, err := s.repo.Update(r.Context(), id, title, enabled)
	if err != nil {
		s.fail(w, ip, "ssh_keys_update", err, "更新 SSH 公钥失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "ssh_keys_update", ip, true,
		"id="+strconv.FormatInt(id, 10)+" enabled="+strconv.FormatBool(enabled))
	httpx.WriteJSON(w, http.StatusOK, viewOf(key))
}

// Delete 处理 DELETE /api/admin/ssh-keys/:id：删除后 sshd 的下一次查询立即看不到该公钥。
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	id, err := keyID(r)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.repo.Delete(r.Context(), id); err != nil {
		s.fail(w, ip, "ssh_keys_delete", err, "删除 SSH 公钥失败")
		return
	}
	util.Audit(s.cfg.AuditLog, "ssh_keys_delete", ip, true, "id="+strconv.FormatInt(id, 10))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyID 解析路径参数并拒绝非正整数。
func keyID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(httpx.Param(r, "id")), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("公钥 ID 非法")
	}
	return id, nil
}

// fail 把仓储错误映射成 HTTP 响应，并保证日志/审计不含公钥内容。
func (s *Service) fail(w http.ResponseWriter, ip, action string, err error, message string) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "公钥不存在")
	case errors.Is(err, ErrUnavailable):
		httpx.Error(w, http.StatusServiceUnavailable, "SSH 公钥库不可用")
	default:
		if s.log != nil {
			s.log.Warn("SSH 公钥操作失败", "action", action, "err", err.Error())
		}
		util.Audit(s.cfg.AuditLog, action, ip, false, err.Error())
		httpx.Error(w, http.StatusInternalServerError, message)
	}
}
