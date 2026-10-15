package notifications

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 附件约束（与旧实现 attachments.ts 常量一致）。
const (
	attachmentMaxCount      = 4
	attachmentMaxFileBytes  = 8 * 1024 * 1024
	attachmentMaxTotalBytes = 20 * 1024 * 1024
)

// allowedAttachmentMimes 是 mime 白名单（其它一律 400）。
// text/html 供「归档报告网页」：正文应是可阅读的真实网页，而不是网页截图。
var allowedAttachmentMimes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/webp":      true,
	"image/gif":       true,
	"application/pdf": true,
	"text/markdown":   true,
	"text/plain":      true,
	"text/html":       true,
}

// preparedAttachment 是通过校验、待落盘的附件。
type preparedAttachment struct {
	name string
	mime string
	data []byte
}

// parseResult 是附件解析结果：ok=false 时携带应回给客户端的 status/error。
type parseResult struct {
	ok     bool
	status int
	err    string
	items  []preparedAttachment
}

func parseError(status int, msg string) parseResult {
	return parseResult{ok: false, status: status, err: msg}
}

// attachmentDir 返回某条通知的附件目录（notification-files/<id>）。
func (s *Service) attachmentDir(id int64) string {
	return filepath.Join(s.cfg.NotificationFilesDir, strconv.FormatInt(id, 10))
}

// sanitizeAttachmentName 仅保留 basename，非法字符替换为 `_`，去首部 `.`，长度 ≤ 80，空名回退 `file`。
// 结果绝不含路径分隔符；客户端给的路径一律不参与。
func sanitizeAttachmentName(raw string) string {
	name := strings.ReplaceAll(raw, "\\", "/")
	parts := strings.Split(name, "/")
	base := parts[len(parts)-1]
	var b strings.Builder
	for _, r := range base {
		if r < utf8.RuneSelf && (r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	safe := b.String()
	if len(safe) > 80 {
		safe = safe[:80]
	}
	safe = strings.TrimLeft(safe, ".")
	if safe == "" {
		return "file"
	}
	return safe
}

// dedupeName 在同一通知内为重名文件加 `-2`、`-3`… 后缀（保留扩展名），并维持长度 ≤ 80。
func dedupeName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	dot := strings.LastIndex(name, ".")
	stem := name
	ext := ""
	if dot > 0 {
		stem = name[:dot]
		ext = name[dot:]
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		room := 80 - len(suffix) - len(ext)
		if room < 1 {
			room = 1
		}
		if room > len(stem) {
			room = len(stem)
		}
		candidate := stem[:room] + suffix + ext
		if !used[candidate] {
			return candidate
		}
	}
}

// stripDataURLPrefix 去掉可能的 data URL 前缀（`data:image/png;base64,`），只留纯 base64。
func stripDataURLPrefix(s string) string {
	if idx := strings.Index(s, "base64,"); idx >= 0 {
		return s[idx+len("base64,"):]
	}
	return s
}

// decodeBase64Lenient 宽松解码 base64：忽略字母表外字符，兼容 `-_`（URL 安全）与缺失的填充，
// 对齐 Node Buffer.from(x,'base64') 不抛异常的行为。
func decodeBase64Lenient(s string) ([]byte, bool) {
	buf := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=':
			buf = append(buf, c)
		case c == '-':
			buf = append(buf, '+')
		case c == '_':
			buf = append(buf, '/')
		}
	}
	if len(buf) == 0 {
		return nil, false
	}
	if m := len(buf) % 4; m != 0 {
		buf = append(buf, []byte("====")[:4-m]...)
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(buf)))
	n, err := base64.StdEncoding.Decode(out, buf)
	if err != nil {
		return nil, false
	}
	return out[:n], true
}

// parseAttachments 校验并解码请求里的 attachments。
// 超限（数量/单文件/合计）→ 413 `附件过大`；mime 不在白名单或结构非法 → 400。
// 返回的 name 已安全化并做请求内去重。
func parseAttachments(input any) parseResult {
	switch v := input.(type) {
	case nil:
		return parseResult{ok: true, items: []preparedAttachment{}}
	case []any:
		if len(v) > attachmentMaxCount {
			return parseError(413, "附件过大")
		}
		used := map[string]bool{}
		items := make([]preparedAttachment, 0, len(v))
		total := 0
		for _, raw := range v {
			rec, ok := raw.(map[string]any)
			if !ok {
				return parseError(400, "附件格式无效")
			}
			mime, _ := rec["mime"].(string)
			mime = strings.ToLower(strings.TrimSpace(mime))
			if !allowedAttachmentMimes[mime] {
				return parseError(400, "不支持的附件类型")
			}
			dataStr, _ := rec["dataBase64"].(string)
			dataStr = stripDataURLPrefix(strings.TrimSpace(dataStr))
			if dataStr == "" {
				return parseError(400, "附件数据无效")
			}
			data, ok := decodeBase64Lenient(dataStr)
			if !ok || len(data) == 0 {
				return parseError(400, "附件数据无效")
			}
			if len(data) > attachmentMaxFileBytes {
				return parseError(413, "附件过大")
			}
			total += len(data)
			if total > attachmentMaxTotalBytes {
				return parseError(413, "附件过大")
			}
			rawName, _ := rec["name"].(string)
			name := dedupeName(sanitizeAttachmentName(rawName), used)
			used[name] = true
			items = append(items, preparedAttachment{name: name, mime: mime, data: data})
		}
		return parseResult{ok: true, items: items}
	default:
		return parseError(400, "附件格式无效")
	}
}

// saveNotificationAttachments 落盘附件并写元信息。同一通知为覆盖式（先清旧附件），
// 单批内重名自动加后缀。单个附件失败只记 warning，不抛出、不影响其余附件与通知本身。
func (s *Service) saveNotificationAttachments(ctx context.Context, id int64, items []preparedAttachment) (metas []AttachmentMeta, warnings []string) {
	metas = []AttachmentMeta{}
	warnings = []string{}
	if len(items) == 0 {
		return metas, warnings
	}
	if s.db == nil {
		return metas, []string{"附件目录不可写"}
	}
	s.deleteNotificationAttachments(ctx, []int64{id})

	dir := s.attachmentDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Warn("通知附件目录创建失败", "id", id, "error", err)
		return metas, []string{"附件目录不可写"}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		s.log.Warn("通知附件目录授权失败", "id", id, "error", err)
		return metas, []string{"附件目录不可写"}
	}

	used := map[string]bool{}
	for _, item := range items {
		name := dedupeName(item.name, used)
		used[name] = true
		full := filepath.Join(dir, name)
		if err := writeFile0600(full, item.data); err != nil {
			// 回给客户端的 warning 不含系统路径，细节只进服务端日志
			warnings = append(warnings, "附件写入失败: "+name)
			s.log.Warn("通知附件写入失败", "id", id, "name", name, "error", err)
			continue
		}
		if _, err := s.db.SQL.ExecContext(ctx, s.attachmentUpsertSQL(), id, name, item.mime, len(item.data)); err != nil {
			warnings = append(warnings, "附件写入失败: "+name)
			s.log.Warn("通知附件元信息落库失败", "id", id, "name", name, "error", err)
			continue
		}
		metas = append(metas, AttachmentMeta{Name: name, Mime: item.mime, Size: len(item.data)})
	}
	return metas, warnings
}

// writeFile0600 创建文件并强制 0600（先按 0600 建再 chmod，规避 umask）。
func writeFile0600(fullPath string, data []byte) error {
	f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(fullPath, 0o600)
}

// attachmentUpsertSQL 返回附件元信息写入语句。主键为 (notification_id, name)，
// MySQL 的 REPLACE INTO 等价于「删除+插入」，对无外键引用的附件表是幂等 upsert。
func (s *Service) attachmentUpsertSQL() string {
	return "REPLACE INTO notification_attachments (notification_id, name, mime, size) VALUES (?, ?, ?, ?)"
}

// listAttachments 返回单条通知的附件元信息（按 name 升序）。
func (s *Service) listAttachments(ctx context.Context, id int64) ([]AttachmentMeta, error) {
	rows, err := s.db.SQL.QueryContext(ctx,
		"SELECT name, mime, size FROM notification_attachments WHERE notification_id = ? ORDER BY name ASC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttachmentMeta{}
	for rows.Next() {
		var m AttachmentMeta
		if err := rows.Scan(&m.Name, &m.Mime, &m.Size); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachmentsForIds 批量取附件元信息（列表页一次查清，避免逐条读盘）。
func (s *Service) attachmentsForIds(ctx context.Context, ids []int64) (map[int64][]AttachmentMeta, error) {
	got := map[int64][]AttachmentMeta{}
	if len(ids) == 0 {
		return got, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.SQL.QueryContext(ctx,
		"SELECT notification_id, name, mime, size FROM notification_attachments WHERE notification_id IN ("+placeholders(len(ids))+") ORDER BY notification_id ASC, name ASC",
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m AttachmentMeta
		if err := rows.Scan(&id, &m.Name, &m.Mime, &m.Size); err != nil {
			return nil, err
		}
		got[id] = append(got[id], m)
	}
	return got, rows.Err()
}

// attachmentURL 返回附件下载地址（同源相对路径；前端直接使用不要再拼）。
func attachmentURL(id int64, name string) string {
	return "/api/admin/notifications/" + strconv.FormatInt(id, 10) + "/files/" + url.PathEscape(name)
}

// toAttachmentViews 把元信息映射为带 url 的接口视图；空输入返回非 nil 空切片。
func toAttachmentViews(id int64, metas []AttachmentMeta) []AttachmentView {
	out := make([]AttachmentView, 0, len(metas))
	for _, m := range metas {
		out = append(out, AttachmentView{Name: m.Name, Mime: m.Mime, Size: m.Size, URL: attachmentURL(id, m.Name)})
	}
	return out
}

// hasImageAttachment 判断是否存在图片附件（前端列表显示图片标记用）。
func hasImageAttachment(metas []AttachmentMeta) bool {
	for _, m := range metas {
		if strings.HasPrefix(m.Mime, "image/") {
			return true
		}
	}
	return false
}

// htmlAttachmentDownloadHeaders 是归档 HTML 的安全响应头：只给 sandbox（不带 allow-scripts /
// allow-same-origin），并配 nosniff 禁止 MIME 嗅探，绝不让其以同源身份执行脚本。
var htmlAttachmentDownloadHeaders = map[string]string{
	"Content-Security-Policy": "sandbox",
	"X-Content-Type-Options":  "nosniff",
}

// attachmentDownloadHeaders 仅对 text/html 附加 CSP sandbox + nosniff，其它 mime 返回空。
func attachmentDownloadHeaders(mime string) map[string]string {
	if mime == "text/html" {
		out := make(map[string]string, len(htmlAttachmentDownloadHeaders))
		for k, v := range htmlAttachmentDownloadHeaders {
			out[k] = v
		}
		return out
	}
	return map[string]string{}
}

// safeDecode 解码 URI（兼容双重编码）；失败保留原值。
func safeDecode(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil {
		return decoded
	}
	return raw
}

// foundAttachment 是命中的待下载附件。
type foundAttachment struct {
	path string
	name string
	mime string
}

// findAttachment 定位待下载附件：name 取 basename 后与落盘文件名精确匹配，并做路径前缀校验防穿越。
func (s *Service) findAttachment(ctx context.Context, id int64, rawName string) (*foundAttachment, error) {
	decoded := safeDecode(rawName)
	base := path.Base(decoded)
	if base == "" || base == "." || base == ".." || base == "/" {
		return nil, nil
	}
	dir := filepath.Clean(s.attachmentDir(id))
	resolved := filepath.Join(dir, base)
	if resolved != filepath.Join(dir, path.Base(resolved)) || !strings.HasPrefix(resolved, dir+string(filepath.Separator)) {
		return nil, nil
	}
	var mime string
	err := s.db.SQL.QueryRowContext(ctx,
		"SELECT mime FROM notification_attachments WHERE notification_id = ? AND name = ?", id, base).Scan(&mime)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	if _, err := os.Stat(resolved); err != nil {
		return nil, nil
	}
	return &foundAttachment{path: resolved, name: base, mime: mime}, nil
}

// deleteNotificationAttachments 删除若干通知的附件：先删元信息行，再删整个附件目录。
// 失败只 warn，不抛出、不阻断删除通知行。
func (s *Service) deleteNotificationAttachments(ctx context.Context, ids []int64) {
	if len(ids) == 0 || s.db == nil {
		return
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	if _, err := s.db.SQL.ExecContext(ctx,
		"DELETE FROM notification_attachments WHERE notification_id IN ("+placeholders(len(ids))+")", args...); err != nil {
		s.log.Warn("删除通知附件元信息失败（不影响删除通知）", "error", err)
	}
	for _, id := range ids {
		if err := os.RemoveAll(s.attachmentDir(id)); err != nil {
			s.log.Warn("删除通知附件目录失败", "id", id, "error", err)
		}
	}
}
