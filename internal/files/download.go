package files

import (
	"fmt"
	"mime"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"admin-server-go/internal/httpx"
)

// hexEscape 对应旧实现 content-disposition 里的 HEX_ESCAPE_REGEXP。
var hexEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// ServeAttachment 以附件形式流式返回文件，Content-Disposition 按 RFC 5987 编码文件名，
// 其余（Range / Last-Modified / Content-Length）交给 http.ServeContent。
// 导出以便 shares 的公开下载复用同一套响应头与文件名编码。
func (s *Service) ServeAttachment(w http.ResponseWriter, r *http.Request, full, filename string) {
	f, err := os.Open(full)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.Header().Set("Content-Type", mimeTypeForName(filename))
	w.Header().Set("Cache-Control", "public, max-age=0")
	http.ServeContent(w, r, filename, st.ModTime(), f)
}

// mimeTypeForName 按扩展名给出 Content-Type，未知回退 application/octet-stream。
func mimeTypeForName(name string) string {
	ext := strings.ToLower(nodeExtname(name))
	if ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	return "application/octet-stream"
}

// contentDisposition 复刻旧实现依赖的 content-disposition 包：产出
// attachment; filename="..."[; filename*=UTF-8”...]，非 Latin-1 文件名同时给回退名。
func contentDisposition(filename string) string {
	name := posixBasename(filename)
	fallbackName := getLatin1(name)
	hasFallback := fallbackName != name
	isQuotedString := isText(name)

	type param struct{ key, val string }
	params := make([]param, 0, 2)
	if hasFallback || !isQuotedString || hexEscape.MatchString(name) {
		params = append(params, param{"filename*", name})
	}
	if isQuotedString || hasFallback {
		val := name
		if hasFallback {
			val = fallbackName
		}
		params = append(params, param{"filename", val})
	}
	sort.Slice(params, func(i, j int) bool { return params[i].key < params[j].key })

	var b strings.Builder
	b.WriteString("attachment")
	for _, p := range params {
		b.WriteString("; ")
		b.WriteString(p.key)
		b.WriteByte('=')
		if strings.HasSuffix(p.key, "*") {
			b.WriteString(ustring(p.val))
		} else {
			b.WriteString(qstring(p.val))
		}
	}
	return b.String()
}

// getLatin1 复刻 content-disposition 的 getlatin1：把超出 Latin-1 可打印范围的字符替换为 '?'。
func getLatin1(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 0x20 && r <= 0x7e) || (r >= 0xa0 && r <= 0xff) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// isText 对应 content-disposition 的 TEXT_REGEXP：非空且全部落在可安全放进 quoted-string 的范围。
func isText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r > 0xff {
			return false
		}
	}
	return true
}

// qstring 复刻 content-disposition 的 qstring：转义反斜杠与引号后加双引号。
func qstring(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

// ustring 复刻 content-disposition 的 ustring：RFC 5987 的 UTF-8” 百分号编码。
func ustring(s string) string {
	var b strings.Builder
	b.WriteString("UTF-8''")
	for _, r := range s {
		if isUnreservedURIComponent(r) {
			// encodeURIComponent 保留这些字符，但紧随其后的正则会把 * ' ( ) 再编码一次。
			switch r {
			case '*', '\'', '(', ')':
				fmt.Fprintf(&b, "%%%02X", r)
			default:
				b.WriteRune(r)
			}
			continue
		}
		for _, by := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", by)
		}
	}
	return b.String()
}

// isUnreservedURIComponent 对应 encodeURIComponent 不编码的字符集合。
func isUnreservedURIComponent(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}
