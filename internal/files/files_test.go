package files

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestService 造一个以临时目录为根的文件区服务。
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		FileDir:   filepath.Join(root, "files"),
		UploadDir: filepath.Join(root, "uploads"),
		AuditLog:  filepath.Join(root, "audit.log"),
		Port:      19265,
	}
	return New(Options{Cfg: cfg, Log: discardLogger()}), root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func contained(full, dir string) bool {
	return full == dir || strings.HasPrefix(full, dir+string(os.PathSeparator))
}

func TestResolveRelTraversal(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.FileDir()

	cases := []struct {
		in     string
		wantOK bool
	}{
		{"notes/a.txt", true},
		{"", true},
		{".", true},
		{"../etc/passwd", false},
		{"a/../../b", false},
		{"..", false},
		{"foo/../../../bar", false},
		{"%2e%2e%2fsecret", false},
		{"%2e%2e", false},
		{"%252e%252e%252fsecret", true}, // 单次解码后仍是字面量，始终落在 FILE_DIR 内
		{"/etc/passwd", true},           // 多余前导斜杠被剥离，收拢进 FILE_DIR
		{"%2Fetc%2Fpasswd", true},
		{"%zz", false},         // 非法转义
		{"%ED%A0%80", false},   // 解码结果非法 UTF-8
		{"a\\..\\..\\b", true}, // 反斜杠在 POSIX 下是普通字符，不构成越界
	}
	for _, c := range cases {
		full, ok := s.ResolveRel(c.in)
		if ok != c.wantOK {
			t.Errorf("ResolveRel(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && !contained(full, dir) {
			t.Errorf("ResolveRel(%q) = %q 越出 %q", c.in, full, dir)
		}
	}
}

func TestIsProtected(t *testing.T) {
	s, root := newTestService(t)
	dir := s.FileDir()
	mustMkdir(t, filepath.Join(dir, "swapfile"))
	mustMkdir(t, filepath.Join(dir, "toolchains", "sub"))
	mustWrite(t, filepath.Join(dir, "my-swapfile.txt"), "x")
	mustMkdir(t, filepath.Join(dir, "sub", "swapfile"))

	cases := []struct {
		full string
		want bool
	}{
		{dir, false},
		{filepath.Join(dir, "swapfile"), true},
		{filepath.Join(dir, "swapfile", "x"), true},
		{filepath.Join(dir, "cache", "y", "z"), true},
		{filepath.Join(dir, "toolchains"), true},
		{filepath.Join(dir, "toolchains", "sub"), false},
		{filepath.Join(dir, "my-swapfile.txt"), false},
		{filepath.Join(dir, "sub", "swapfile"), false},
		{filepath.Join(root, "outside"), false},
	}
	for _, c := range cases {
		if got := s.IsProtected(c.full); got != c.want {
			t.Errorf("IsProtected(%q) = %v, want %v", c.full, got, c.want)
		}
	}
}

func TestIsHiddenRoot(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.FileDir()
	cases := []struct {
		full string
		want bool
	}{
		{filepath.Join(dir, "swapfile"), true},
		{filepath.Join(dir, "cache", "x"), true},
		{filepath.Join(dir, "toolchains"), false}, // 只读目录仍展示
		{filepath.Join(dir, "sub", "swapfile"), false},
		{dir, false},
	}
	for _, c := range cases {
		if got := s.isHiddenRoot(c.full); got != c.want {
			t.Errorf("isHiddenRoot(%q) = %v, want %v", c.full, got, c.want)
		}
	}
}

func TestSanitizeSegmentAndExtname(t *testing.T) {
	sanitize := []struct{ in, want string }{
		{"a/b.txt", "b.txt"},
		{"../evil", "evil"},
		{"..", ""},
		{".", ""},
		{"", ""},
		{"  name  ", "name"},
		{"bad\u0000name", "badname"},
		{"中.txt", "-.txt"}, // 复刻旧实现 latin1→utf8 的截断怪癖
		{latin1Name("中.txt"), "中.txt"},
		{latin1Name("测试文件.txt"), "测试文件.txt"},
	}
	for _, c := range sanitize {
		if got := sanitizeSegment(c.in); got != c.want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	ext := []struct{ in, want string }{
		{"notes.7z", ".7z"},
		{"archive.tar.gz", ".gz"},
		{".bashrc", ""},
		{"a.", "."},
		{"..", ""},
		{"...", "."},
		{".a.b", ".b"},
		{"noext", ""},
	}
	for _, c := range ext {
		if got := nodeExtname(c.in); got != c.want {
			t.Errorf("nodeExtname(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUniqueFileName(t *testing.T) {
	s, _ := newTestService(t)
	dir := filepath.Join(s.FileDir(), "unique")
	mustMkdir(t, dir)

	if got := uniqueFileName(dir, "notes.7z"); got != "notes.7z" {
		t.Fatalf("first = %q", got)
	}
	mustWrite(t, filepath.Join(dir, "notes.7z"), "a")
	if got := uniqueFileName(dir, "notes.7z"); got != "notes-2.7z" {
		t.Fatalf("second = %q", got)
	}
	mustWrite(t, filepath.Join(dir, "notes-2.7z"), "a")
	if got := uniqueFileName(dir, "notes.7z"); got != "notes-3.7z" {
		t.Fatalf("third = %q", got)
	}
	if got := uniqueFileName(dir, ""); got != "unnamed" {
		t.Fatalf("empty = %q", got)
	}
	if got := uniqueFileName(dir, latin1Name("中.txt")); got != "中.txt" {
		t.Fatalf("chinese = %q", got)
	}
}

func TestContentDisposition(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain.txt", `attachment; filename="plain.txt"`},
		{"中.txt", `attachment; filename="?.txt"; filename*=UTF-8''%E4%B8%AD.txt`},
		{"a b.txt", `attachment; filename="a b.txt"`},
		{"quote\"name.txt", `attachment; filename="quote\"name.txt"`},
		{"a%20b.txt", `attachment; filename="a%20b.txt"; filename*=UTF-8''a%2520b.txt`},
		{"noext", `attachment; filename="noext"`},
	}
	for _, c := range cases {
		if got := contentDisposition(c.in); got != c.want {
			t.Errorf("contentDisposition(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func multipartBody(t *testing.T, field, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestFilesUploadKeepsChineseNameAndDedupes(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.FileDir()

	do := func() map[string]any {
		// 浏览器把 UTF-8 文件名原样写进 Content-Disposition；Go 的 multipart reader
		// 会把原始字节作为字符串返回，服务内部再走 latin1→utf8 还原。
		body, ct := multipartBody(t, "file", "中文文件.txt", "hello")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/files/upload?path=sub", body)
		req.Header.Set("Content-Type", ct)
		rr := httptest.NewRecorder()
		s.FilesUpload(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		return decodeObject(t, rr.Body.Bytes())
	}

	first := do()
	if first["name"] != "中文文件.txt" {
		t.Fatalf("first name = %v", first["name"])
	}
	second := do()
	if second["name"] != "中文文件-2.txt" {
		t.Fatalf("second name = %v", second["name"])
	}
	content, err := os.ReadFile(filepath.Join(dir, "sub", "中文文件.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("stored content = %q err=%v", content, err)
	}
}

func TestFilesUploadRejectsProtectedRoot(t *testing.T) {
	s, _ := newTestService(t)
	mustMkdir(t, filepath.Join(s.FileDir(), "drop"))

	body, ct := multipartBody(t, "file", "secret.txt", "x")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/files/upload?path=drop", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FilesUpload(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.FileDir(), "drop", "secret.txt")); !os.IsNotExist(err) {
		t.Fatalf("受保护文件未被清理: err=%v", err)
	}
}

func TestReceiveFileSizeLimit(t *testing.T) {
	s, _ := newTestService(t)
	dir := filepath.Join(s.FileDir(), "limit")
	mustMkdir(t, dir)

	body, ct := multipartBody(t, "file", "big.bin", strings.Repeat("a", 100))
	req := httptest.NewRequest(http.MethodPost, "/api/admin/files/upload", body)
	req.Header.Set("Content-Type", ct)

	_, err := s.receiveFile(req, 10, func(string) (string, string, error) {
		return dir, "big.bin", nil
	})
	if err != errTooLarge {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("超大文件未清理: %v", statErr)
	}
}

func TestFilesListOrderingAndFiltering(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.FileDir()
	mustMkdir(t, filepath.Join(dir, "bdir"))
	mustMkdir(t, filepath.Join(dir, "adir"))
	mustMkdir(t, filepath.Join(dir, "toolchains"))
	mustMkdir(t, filepath.Join(dir, "cache"))
	mustWrite(t, filepath.Join(dir, "cache", "hidden.txt"), "x")
	mustWrite(t, filepath.Join(dir, "a.txt"), "x")
	if err := os.Symlink(filepath.Join(dir, "a.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/files?path=", nil)
	rr := httptest.NewRecorder()
	s.FilesList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	obj := decodeObject(t, rr.Body.Bytes())
	if obj["parent"] != nil {
		t.Errorf("root parent = %v, want null", obj["parent"])
	}
	entries := obj["entries"].([]any)
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		m := e.(map[string]any)
		got = append(got, m["name"].(string))
	}
	want := []string{"adir", "bdir", "toolchains", "a.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestFilesListTraversalAndNotDir(t *testing.T) {
	s, _ := newTestService(t)
	mustWrite(t, filepath.Join(s.FileDir(), "a.txt"), "x")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/files?path=..%2Fetc", nil)
	rr := httptest.NewRecorder()
	s.FilesList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/files?path=a.txt", nil)
	rr = httptest.NewRecorder()
	s.FilesList(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("not-a-dir status = %d", rr.Code)
	}
}

func TestMkdirRenameDelete(t *testing.T) {
	s, _ := newTestService(t)
	dir := s.FileDir()

	call := func(method, target string, body map[string]any) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if body != nil {
			req = httpx.WithBody(req, body)
		}
		rr := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(target, "/mkdir"):
			s.FilesMkdir(rr, req)
		case strings.HasSuffix(target, "/rename"):
			s.FilesRename(rr, req)
		default:
			s.FilesDelete(rr, req)
		}
		return rr
	}

	if rr := call(http.MethodPost, "/api/admin/files/mkdir", map[string]any{"path": "", "name": "newdir"}); rr.Code != http.StatusOK {
		t.Fatalf("mkdir status = %d (%s)", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir")); err != nil {
		t.Fatalf("newdir missing: %v", err)
	}
	if rr := call(http.MethodPost, "/api/admin/files/mkdir", map[string]any{"path": "", "name": "newdir"}); rr.Code != http.StatusConflict {
		t.Fatalf("dup mkdir status = %d", rr.Code)
	}
	if rr := call(http.MethodPost, "/api/admin/files/mkdir", map[string]any{"path": "", "name": ".."}); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad name status = %d", rr.Code)
	}
	if rr := call(http.MethodPost, "/api/admin/files/mkdir", map[string]any{"path": "", "name": "cache"}); rr.Code != http.StatusForbidden {
		t.Fatalf("protected mkdir status = %d", rr.Code)
	}

	mustWrite(t, filepath.Join(dir, "newdir", "a.txt"), "x")
	if rr := call(http.MethodPost, "/api/admin/files/rename", map[string]any{"path": "newdir/a.txt", "name": "b.txt"}); rr.Code != http.StatusOK {
		t.Fatalf("rename status = %d (%s)", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir", "b.txt")); err != nil {
		t.Fatalf("renamed missing: %v", err)
	}
	if rr := call(http.MethodDelete, "/api/admin/files?path=newdir", nil); rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rr.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "newdir")); !os.IsNotExist(err) {
		t.Fatalf("newdir not deleted")
	}
}

func TestFilesDownload(t *testing.T) {
	s, _ := newTestService(t)
	mustWrite(t, filepath.Join(s.FileDir(), "中文.txt"), "hello")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/files/download?path="+urlEscape("中文.txt"), nil)
	rr := httptest.NewRecorder()
	s.FilesDownload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	wantCD := `attachment; filename="??.txt"; filename*=UTF-8''%E4%B8%AD%E6%96%87.txt`
	if got := rr.Header().Get("Content-Disposition"); got != wantCD {
		t.Fatalf("Content-Disposition = %q, want %q", got, wantCD)
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("body = %q", rr.Body.String())
	}
}

func TestGenericDownloadWhitelist(t *testing.T) {
	s, _ := newTestService(t)
	mustWrite(t, filepath.Join(s.UploadDir(), "ok.txt"), "data")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/download?path="+urlEscape(filepath.Join(s.UploadDir(), "ok.txt")), nil)
	rr := httptest.NewRecorder()
	s.Download(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "data" {
		t.Fatalf("status = %d body = %q", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/download?path="+urlEscape("/etc/hostname"), nil)
	rr = httptest.NewRecorder()
	s.Download(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("outside status = %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/download", nil)
	rr = httptest.NewRecorder()
	s.Download(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing path status = %d", rr.Code)
	}
}

// decodeObject 把 JSON 响应体解码为 map，供断言使用。
func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	obj, err := httpx.ParseBody("application/json", 1<<20, body)
	if err != nil {
		t.Fatalf("decode json: %v (%s)", err, body)
	}
	return obj
}

// urlEscape 用 PathUnescape 的逆运算编码 query 值里的特殊字符（仅用于测试构造 URL）。
func urlEscape(s string) string {
	var b strings.Builder
	for _, by := range []byte(s) {
		if (by >= 'a' && by <= 'z') || (by >= 'A' && by <= 'Z') || (by >= '0' && by <= '9') ||
			by == '-' || by == '_' || by == '.' || by == '~' || by == '/' {
			b.WriteByte(by)
			continue
		}
		const hex = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hex[by>>4])
		b.WriteByte(hex[by&0x0f])
	}
	return b.String()
}
