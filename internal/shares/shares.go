// Package shares 实现文件区临时链接（限时分享）：创建 / 列表 / 改期 / 撤销 / 删除，以及公开下载 /s/:token。
//
// 行为逐条对齐旧实现 src/routes/shares.ts：账本为本地 JSON，status 不落盘，读取时按
// revokedAt / expiresAt 推导；expiresAt=0 表示永久；revoked 不可逆，仅 active 可改期；
// 公开下载对 revoked/expired 返回 410、缺失返回 404，均以极简 HTML 提示页呈现。
package shares

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"admin-server-go/internal/config"
	"admin-server-go/internal/files"
)

const (
	// sharePermanent 是 expiresAt 的「永久有效」哨兵值。
	sharePermanent = 0
	// shareDefaultTTLHours 是未指定有效期时的默认时长（小时）。
	shareDefaultTTLHours = 24
	// shareMinTTLHours / shareMaxTTLHours 是 ttlHours 的夹紧区间（1 小时 ~ 1 年）。
	shareMinTTLHours = 1
	shareMaxTTLHours = 8760
	// shareRevokedKeepMs / shareExpiredKeepMs 是惰性清理的保留时长。
	shareRevokedKeepMs = int64(30) * 24 * 3600 * 1000
	shareExpiredKeepMs = int64(90) * 24 * 3600 * 1000
	// noteMaxRunes 是备注截断长度。
	noteMaxRunes = 500
)

// Options 是构造 Service 的依赖。
type Options struct {
	Cfg   *config.Config
	Files *files.Service
	Log   *slog.Logger
}

// Service 持有分享账本路径、分享基址与文件区辅助服务。
type Service struct {
	cfg       *config.Config
	files     *files.Service
	log       *slog.Logger
	shareFile string
	baseURL   string
	// mu 串行化账本的读-改-写；旧实现是单线程 Node，Go 下需显式加锁以免并发写坏账本。
	mu sync.Mutex
}

// New 构造分享服务。
func New(opts Options) *Service {
	return &Service{
		cfg:       opts.Cfg,
		files:     opts.Files,
		log:       opts.Log,
		shareFile: opts.Cfg.ShareFile,
		baseURL:   opts.Cfg.ShareBaseURL,
	}
}

// shareRecord 是账本里的一条记录（data/file-shares.json 数组元素）。
type shareRecord struct {
	ID           string  `json:"id"`
	Token        string  `json:"token"`
	RelPath      string  `json:"relPath"`
	Size         int64   `json:"size"`
	Note         string  `json:"note"`
	CreatedAt    int64   `json:"createdAt"`
	ExpiresAt    int64   `json:"expiresAt"`
	RevokedAt    *int64  `json:"revokedAt"`
	Downloads    int64   `json:"downloads"`
	LastAccessAt *int64  `json:"lastAccessAt"`
	LastAccessIP *string `json:"lastAccessIp"`
}

// shareView 是面向接口的分享视图（存储字段 + 计算字段）。
type shareView struct {
	ID           string  `json:"id"`
	Token        string  `json:"token"`
	RelPath      string  `json:"relPath"`
	Size         int64   `json:"size"`
	Note         string  `json:"note"`
	CreatedAt    int64   `json:"createdAt"`
	ExpiresAt    int64   `json:"expiresAt"`
	RevokedAt    *int64  `json:"revokedAt"`
	Downloads    int64   `json:"downloads"`
	LastAccessAt *int64  `json:"lastAccessAt"`
	LastAccessIP *string `json:"lastAccessIp"`
	URL          string  `json:"url"`
	Status       string  `json:"status"`
	RemainingMs  *int64  `json:"remainingMs"`
	FileExists   bool    `json:"fileExists"`
}

// shareStatus 推导记录状态（不落盘）：revoked 优先，其次永久，其次按到期时刻判定。
func (s *Service) shareStatus(rec shareRecord, now int64) string {
	if rec.RevokedAt != nil && *rec.RevokedAt != 0 {
		return "revoked"
	}
	if rec.ExpiresAt == sharePermanent {
		return "active"
	}
	if rec.ExpiresAt <= now {
		return "expired"
	}
	return "active"
}

// readRaw 读取账本原始数组：文件不存在 / 损坏返回空；非对象元素被丢弃（对应旧实现的宽松校验）。
func (s *Service) readRaw() []shareRecord {
	raw, err := os.ReadFile(s.shareFile)
	if err != nil {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]shareRecord, 0, len(items))
	for _, item := range items {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		var rec shareRecord
		if err := json.Unmarshal(trimmed, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// writeShares 原子写账本（临时文件 + rename），关闭 HTML 转义并保持两空格缩进，对齐旧实现 JSON.stringify。
func (s *Service) writeShares(list []shareRecord) {
	if err := os.MkdirAll(filepath.Dir(s.shareFile), 0o700); err != nil {
		s.warn("分享账本写入失败", err)
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(list); err != nil {
		s.warn("分享账本写入失败", err)
		return
	}
	body := bytes.TrimRight(buf.Bytes(), "\n")
	tmp := s.shareFile + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		s.warn("分享账本写入失败", err)
		return
	}
	if err := os.Rename(tmp, s.shareFile); err != nil {
		s.warn("分享账本写入失败", err)
	}
}

// readShares 读取账本并惰性清理：revoked 超 30 天 / expired 超 90 天自动删除（有变化才写回）。
func (s *Service) readShares() []shareRecord {
	now := config.NowMillis()
	list := s.readRaw()
	kept := make([]shareRecord, 0, len(list))
	for _, rec := range list {
		switch s.shareStatus(rec, now) {
		case "revoked":
			if rec.RevokedAt != nil && *rec.RevokedAt != 0 && now-*rec.RevokedAt > shareRevokedKeepMs {
				continue
			}
		case "expired":
			if rec.ExpiresAt > 0 && now-rec.ExpiresAt > shareExpiredKeepMs {
				continue
			}
		}
		kept = append(kept, rec)
	}
	if len(kept) != len(list) {
		s.writeShares(kept)
	}
	return kept
}

// shareBaseURL 推导分享基址：环境变量优先，否则由 X-Forwarded-Proto/Host 推导，禁止硬编码域名。
func (s *Service) shareBaseURL(r *http.Request) string {
	if s.baseURL != "" {
		return s.baseURL
	}
	proto := firstToken(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = "http"
	}
	host := firstToken(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		// r.Host 即请求的 Host 头；X-Forwarded-Host 缺失时回退到它。
		host = firstToken(r.Host)
	}
	if host == "" {
		host = fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	}
	return proto + "://" + host
}

// toShareView 把记录转换为列表视图。
func (s *Service) toShareView(rec shareRecord, r *http.Request, now int64) shareView {
	status := s.shareStatus(rec, now)
	var remaining *int64
	switch {
	case status != "active":
		zero := int64(0)
		remaining = &zero
	case rec.ExpiresAt == sharePermanent:
		remaining = nil
	default:
		v := rec.ExpiresAt - now
		if v < 0 {
			v = 0
		}
		remaining = &v
	}
	full, ok := s.shareFullFromRel(rec.RelPath)
	return shareView{
		ID:           rec.ID,
		Token:        rec.Token,
		RelPath:      rec.RelPath,
		Size:         rec.Size,
		Note:         rec.Note,
		CreatedAt:    rec.CreatedAt,
		ExpiresAt:    rec.ExpiresAt,
		RevokedAt:    normalizedTime(rec.RevokedAt),
		Downloads:    rec.Downloads,
		LastAccessAt: rec.LastAccessAt,
		LastAccessIP: rec.LastAccessIP,
		URL:          fmt.Sprintf("%s/s/%s", s.shareBaseURL(r), rec.Token),
		Status:       status,
		RemainingMs:  remaining,
		FileExists:   shareFileInfo(full, ok).kind == "file",
	}
}

// resolveSharePath 规范化创建入参：仅接受 FILE_DIR 内的相对路径，绝对路径 / 越界 / 含 .. 一律拒绝。
func (s *Service) resolveSharePath(raw any) (string, bool) {
	str, ok := raw.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(str)
	if trimmed == "" {
		return "", false
	}
	decoded, ok := decodeURIComponent(trimmed)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(decoded, "/") || strings.HasPrefix(decoded, "\\") {
		return "", false
	}
	if len(decoded) >= 3 && isASCIILetter(decoded[0]) && decoded[1] == ':' &&
		(decoded[2] == '/' || decoded[2] == '\\') {
		return "", false
	}
	for _, seg := range strings.FieldsFunc(decoded, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", false
		}
	}
	full, ok := s.files.ResolveRel(trimmed)
	if !ok || full == s.files.FileDir() {
		return "", false
	}
	return full, true
}

// shareFullFromRel 把账本里的 relPath 还原为绝对路径；越界返回 false（防账本被篡改）。
func (s *Service) shareFullFromRel(rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	base := s.files.FileDir()
	var full string
	if filepath.IsAbs(rel) {
		full = filepath.Clean(rel)
	} else {
		full = filepath.Clean(filepath.Join(base, rel))
	}
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// shareFileInfo 用 lstat 判定 file / dir / other / missing。
type fileInfo struct {
	kind string
	size int64
}

func shareFileInfo(full string, ok bool) fileInfo {
	if !ok {
		return fileInfo{kind: "missing"}
	}
	st, err := os.Lstat(full)
	if err != nil {
		return fileInfo{kind: "missing"}
	}
	if st.Mode().IsRegular() {
		return fileInfo{kind: "file", size: st.Size()}
	}
	if st.IsDir() {
		return fileInfo{kind: "dir"}
	}
	return fileInfo{kind: "other"}
}

// normalizedTime 把 0 值指针视为 null（对齐旧实现 `rec.revokedAt || null`）。
func normalizedTime(v *int64) *int64 {
	if v == nil || *v == 0 {
		return nil
	}
	return v
}

// firstToken 取逗号分隔头的第一个 token 并 trim（NS 可能追加多个上游）。
func firstToken(raw string) string {
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(raw, ",")[0])
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// decodeURIComponent 与 JS decodeURIComponent 等价：单次百分号解码，非法转义或非法 UTF-8 返回 false。
func decodeURIComponent(raw string) (string, bool) {
	out, err := url.PathUnescape(raw)
	if err != nil {
		return "", false
	}
	if !utf8.ValidString(out) {
		return "", false
	}
	return out, true
}

func (s *Service) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "err", err)
	}
}

// jsNumber 复刻 JS Number(...) 的取值：数字直接返回，字符串按十进制解析，
// 布尔转 0/1；非有限值返回 false（对应 !Number.isFinite）。
func jsNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return finiteNumber(x)
	case float32:
		return finiteNumber(float64(x))
	case int:
		return finiteNumber(float64(x))
	case int64:
		return finiteNumber(float64(x))
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return finiteNumber(f)
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	default:
		return 0, false
	}
}

func finiteNumber(f float64) (float64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}
