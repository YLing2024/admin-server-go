package shares

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"admin-server-go/internal/config"
	"admin-server-go/internal/files"
	"admin-server-go/internal/httpx"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newShareService 造一个以临时目录为根的分享服务（账本独立于文件区）。
func newShareService(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		FileDir:   filepath.Join(root, "files"),
		UploadDir: filepath.Join(root, "uploads"),
		AuditLog:  filepath.Join(root, "audit.log"),
		ShareFile: filepath.Join(root, "file-shares.json"),
		Port:      19265,
	}
	fs := files.New(files.Options{Cfg: cfg, Log: discardLogger()})
	return New(Options{Cfg: cfg, Files: fs, Log: discardLogger()}), fs.FileDir()
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func decodeObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	obj, err := httpx.ParseBody("application/json", 1<<20, body)
	if err != nil {
		t.Fatalf("decode json: %v (%s)", err, body)
	}
	return obj
}

func withBody(req *http.Request, body map[string]any) *http.Request {
	return httpx.WithBody(req, body)
}

func TestShareStatusStateMachine(t *testing.T) {
	s, _ := newShareService(t)
	now := int64(1_000_000)
	revoked := int64(500)

	cases := []struct {
		name string
		rec  shareRecord
		want string
	}{
		{"permanent", shareRecord{ExpiresAt: 0}, "active"},
		{"future", shareRecord{ExpiresAt: now + 1}, "active"},
		{"expired", shareRecord{ExpiresAt: now - 1}, "expired"},
		{"boundary-equals-now", shareRecord{ExpiresAt: now}, "expired"},
		{"revoked", shareRecord{ExpiresAt: now + 1000, RevokedAt: &revoked}, "revoked"},
		{"revoked-permanent", shareRecord{ExpiresAt: 0, RevokedAt: &revoked}, "revoked"},
	}
	for _, c := range cases {
		if got := s.shareStatus(c.rec, now); got != c.want {
			t.Errorf("%s: shareStatus = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCreateRejectsDirMissingProtectedAndInvalidPath(t *testing.T) {
	s, fileDir := newShareService(t)
	mustWrite(t, filepath.Join(fileDir, "d", "keep"), "x")
	mustWrite(t, filepath.Join(fileDir, "cache", "data"), "x")

	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
		wantMsg  string
	}{
		{"dir", map[string]any{"path": "d"}, http.StatusBadRequest, "暂不支持分享目录"},
		{"missing", map[string]any{"path": "nope.txt"}, http.StatusNotFound, "文件不存在"},
		{"protected", map[string]any{"path": "cache"}, http.StatusForbidden, "该文件受保护，不允许分享"},
		{"absolute", map[string]any{"path": "/etc/passwd"}, http.StatusBadRequest, "路径非法"},
		{"traversal", map[string]any{"path": "../etc/passwd"}, http.StatusBadRequest, "路径非法"},
		{"encoded-traversal", map[string]any{"path": "%2e%2e/etc/passwd"}, http.StatusBadRequest, "路径非法"},
		{"empty", map[string]any{"path": "  "}, http.StatusBadRequest, "路径非法"},
	}
	for _, c := range cases {
		req := withBody(httptest.NewRequest(http.MethodPost, "/api/admin/files/shares", nil), c.body)
		rr := httptest.NewRecorder()
		s.Create(rr, req)
		if rr.Code != c.wantCode {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, rr.Code, c.wantCode, rr.Body.String())
			continue
		}
		if c.wantMsg != "" {
			if got := decodeObject(t, rr.Body.Bytes())["error"]; got != c.wantMsg {
				t.Errorf("%s: error = %v, want %q", c.name, got, c.wantMsg)
			}
		}
	}
}

func TestCreateExpiryValidationAndClamping(t *testing.T) {
	s, fileDir := newShareService(t)
	mustWrite(t, filepath.Join(fileDir, "a.txt"), "hello")
	now := config.NowMillis()

	request := func(body map[string]any) *httptest.ResponseRecorder {
		req := withBody(httptest.NewRequest(http.MethodPost, "/api/admin/files/shares", nil), body)
		rr := httptest.NewRecorder()
		s.Create(rr, req)
		return rr
	}

	if rr := request(map[string]any{"path": "a.txt", "expiresAt": float64(0)}); rr.Code != http.StatusCreated {
		t.Fatalf("permanent status = %d (%s)", rr.Code, rr.Body.String())
	} else if got := int64(decodeObject(t, rr.Body.Bytes())["expiresAt"].(float64)); got != 0 {
		t.Fatalf("permanent expiresAt = %d", got)
	}

	past := request(map[string]any{"path": "a.txt", "expiresAt": float64(now - 1000)})
	if past.Code != http.StatusBadRequest {
		t.Fatalf("past status = %d", past.Code)
	}
	if got := decodeObject(t, past.Body.Bytes())["error"]; got != "expiresAt 须为将来时刻（0 表示永久；立即失效请用撤销）" {
		t.Fatalf("past error = %v", got)
	}

	frac := request(map[string]any{"path": "a.txt", "expiresAt": 1.5})
	if frac.Code != http.StatusBadRequest {
		t.Fatalf("fractional status = %d", frac.Code)
	}
	if got := decodeObject(t, frac.Body.Bytes())["error"]; got != "expiresAt 须为 epoch 毫秒整数（0 表示永久）" {
		t.Fatalf("fractional error = %v", got)
	}

	badTTL := request(map[string]any{"path": "a.txt", "ttlHours": "abc"})
	if badTTL.Code != http.StatusBadRequest {
		t.Fatalf("bad ttl status = %d", badTTL.Code)
	}
	if got := decodeObject(t, badTTL.Body.Bytes())["error"]; got != "ttlHours 须为数字" {
		t.Fatalf("bad ttl error = %v", got)
	}

	assertTTL := func(body map[string]any, wantHours float64) {
		before := config.NowMillis()
		rr := request(body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
		}
		got := int64(decodeObject(t, rr.Body.Bytes())["expiresAt"].(float64))
		want := before + int64(wantHours*3600*1000)
		if diff := got - want; diff < -50 || diff > 500 {
			t.Fatalf("expiresAt drift = %d (got %d want ~%d)", diff, got, want)
		}
	}
	assertTTL(map[string]any{"path": "a.txt"}, shareDefaultTTLHours)
	assertTTL(map[string]any{"path": "a.txt", "ttlHours": float64(0)}, shareMinTTLHours)
	assertTTL(map[string]any{"path": "a.txt", "ttlHours": float64(100000)}, shareMaxTTLHours)
}

func TestShareLifecyclePublicDownload(t *testing.T) {
	s, fileDir := newShareService(t)
	mustWrite(t, filepath.Join(fileDir, "a.txt"), "hello")

	req := withBody(httptest.NewRequest(http.MethodPost, "/api/admin/files/shares", nil), map[string]any{"path": "a.txt"})
	rr := httptest.NewRecorder()
	s.Create(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d (%s)", rr.Code, rr.Body.String())
	}
	view := decodeObject(t, rr.Body.Bytes())
	token := view["token"].(string)
	id := view["id"].(string)
	if view["status"] != "active" || view["relPath"] != "a.txt" || view["downloads"].(float64) != 0 {
		t.Fatalf("view = %v", view)
	}
	wantURL := "http://example.com/s/" + token
	if view["url"] != wantURL {
		t.Fatalf("url = %v, want %s", view["url"], wantURL)
	}
	if view["remainingMs"].(float64) < 0 {
		t.Fatalf("remainingMs = %v", view["remainingMs"])
	}

	public := func(tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/s/"+tok, nil)
		r = httpx.WithParams(r, map[string]string{"token": tok})
		rr := httptest.NewRecorder()
		s.PublicDownload(rr, r)
		return rr
	}

	ok := public(token)
	if ok.Code != http.StatusOK || ok.Body.String() != "hello" {
		t.Fatalf("public status = %d body = %q", ok.Code, ok.Body.String())
	}
	if got := ok.Header().Get("Content-Disposition"); got != `attachment; filename="a.txt"` {
		t.Fatalf("Content-Disposition = %q", got)
	}

	// 下载后计数应为 1
	listReq := httptest.NewRequest(http.MethodGet, "/api/admin/files/shares", nil)
	listRR := httptest.NewRecorder()
	s.List(listRR, listReq)
	shares := decodeObject(t, listRR.Body.Bytes())["shares"].([]any)
	if len(shares) != 1 || shares[0].(map[string]any)["downloads"].(float64) != 1 {
		t.Fatalf("downloads not incremented: %v", shares)
	}

	// 未知 token → 404 HTML
	missing := public("does-not-exist")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "链接不存在") {
		t.Fatalf("missing status = %d body = %q", missing.Code, missing.Body.String())
	}

	// 撤销：不可逆，仅 active 可操作
	upd := withBody(httptest.NewRequest(http.MethodPatch, "/api/admin/files/shares/"+id, nil), map[string]any{"revoked": true})
	upd = httpx.WithParams(upd, map[string]string{"id": id})
	updRR := httptest.NewRecorder()
	s.Update(updRR, upd)
	if updRR.Code != http.StatusOK {
		t.Fatalf("revoke status = %d (%s)", updRR.Code, updRR.Body.String())
	}
	if decodeObject(t, updRR.Body.Bytes())["status"] != "revoked" {
		t.Fatalf("revoked view = %s", updRR.Body.String())
	}

	if got := public(token); got.Code != http.StatusGone || !strings.Contains(got.Body.String(), "链接已失效") {
		t.Fatalf("revoked public status = %d body = %q", got.Code, got.Body.String())
	}

	// 已撤销记录不可再操作
	upd2 := withBody(httptest.NewRequest(http.MethodPatch, "/api/admin/files/shares/"+id, nil), map[string]any{"note": "x"})
	upd2 = httpx.WithParams(upd2, map[string]string{"id": id})
	upd2RR := httptest.NewRecorder()
	s.Update(upd2RR, upd2)
	if upd2RR.Code != http.StatusBadRequest {
		t.Fatalf("revoked update status = %d", upd2RR.Code)
	}
	if got := decodeObject(t, upd2RR.Body.Bytes())["error"]; got != "仅有效链接可操作（当前 revoked）" {
		t.Fatalf("revoked update error = %v", got)
	}
}

func TestUpdateValidation(t *testing.T) {
	s, fileDir := newShareService(t)
	mustWrite(t, filepath.Join(fileDir, "a.txt"), "hello")
	req := withBody(httptest.NewRequest(http.MethodPost, "/api/admin/files/shares", nil), map[string]any{"path": "a.txt", "expiresAt": float64(0)})
	rr := httptest.NewRecorder()
	s.Create(rr, req)
	id := decodeObject(t, rr.Body.Bytes())["id"].(string)

	patch := func(body map[string]any) *httptest.ResponseRecorder {
		r := withBody(httptest.NewRequest(http.MethodPatch, "/api/admin/files/shares/"+id, nil), body)
		r = httpx.WithParams(r, map[string]string{"id": id})
		out := httptest.NewRecorder()
		s.Update(out, r)
		return out
	}

	if got := patch(map[string]any{"revoked": false}); got.Code != http.StatusBadRequest {
		t.Fatalf("restore status = %d", got.Code)
	} else if decodeObject(t, got.Body.Bytes())["error"] != "撤销不可逆，无法恢复" {
		t.Fatalf("restore error = %v", decodeObject(t, got.Body.Bytes())["error"])
	}
	if got := patch(map[string]any{"revoked": "yes"}); got.Code != http.StatusBadRequest {
		t.Fatalf("non-bool status = %d", got.Code)
	} else if decodeObject(t, got.Body.Bytes())["error"] != "revoked 须为布尔值" {
		t.Fatalf("non-bool error = %v", decodeObject(t, got.Body.Bytes())["error"])
	}
	if got := patch(map[string]any{}); got.Code != http.StatusBadRequest {
		t.Fatalf("empty status = %d", got.Code)
	} else if decodeObject(t, got.Body.Bytes())["error"] != "没有可更新的字段" {
		t.Fatalf("empty error = %v", decodeObject(t, got.Body.Bytes())["error"])
	}

	got := patch(map[string]any{"note": "  hello note  "})
	if got.Code != http.StatusOK {
		t.Fatalf("note status = %d (%s)", got.Code, got.Body.String())
	}
	if decodeObject(t, got.Body.Bytes())["note"] != "hello note" {
		t.Fatalf("note = %v", decodeObject(t, got.Body.Bytes())["note"])
	}
}

func TestLedgerLazyCleanup(t *testing.T) {
	s, _ := newShareService(t)
	now := config.NowMillis()
	revokedAt := now - 31*24*3600*1000
	expiredAt := now - 91*24*3600*1000

	active := shareRecord{ID: "keep", Token: "tk", RelPath: "a.txt", ExpiresAt: 0, CreatedAt: now}
	revoked := shareRecord{ID: "rev", Token: "tr", RelPath: "a.txt", ExpiresAt: 0, CreatedAt: now, RevokedAt: &revokedAt}
	expired := shareRecord{ID: "exp", Token: "te", RelPath: "a.txt", ExpiresAt: expiredAt, CreatedAt: now}
	raw, err := json.MarshalIndent([]any{active, revoked, expired, nil}, "", "  ")
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.shareFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.shareFile, raw, 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}

	kept := s.readShares()
	if len(kept) != 1 || kept[0].ID != "keep" {
		t.Fatalf("kept = %+v", kept)
	}
	// 有变化应写回：文件里只剩 active 一条
	after, err := os.ReadFile(s.shareFile)
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	var persisted []shareRecord
	if err := json.Unmarshal(after, &persisted); err != nil {
		t.Fatalf("unmarshal persisted: %v", err)
	}
	if len(persisted) != 1 || persisted[0].ID != "keep" {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestDeleteRemovesRecordOnly(t *testing.T) {
	s, fileDir := newShareService(t)
	mustWrite(t, filepath.Join(fileDir, "a.txt"), "hello")
	req := withBody(httptest.NewRequest(http.MethodPost, "/api/admin/files/shares", nil), map[string]any{"path": "a.txt"})
	rr := httptest.NewRecorder()
	s.Create(rr, req)
	id := decodeObject(t, rr.Body.Bytes())["id"].(string)

	del := httptest.NewRequest(http.MethodDelete, "/api/admin/files/shares/"+id, nil)
	del = httpx.WithParams(del, map[string]string{"id": id})
	delRR := httptest.NewRecorder()
	s.Delete(delRR, del)
	if delRR.Code != http.StatusOK {
		t.Fatalf("delete status = %d", delRR.Code)
	}
	if _, err := os.Stat(filepath.Join(fileDir, "a.txt")); err != nil {
		t.Fatalf("磁盘文件被误删: %v", err)
	}
	del2RR := httptest.NewRecorder()
	s.Delete(del2RR, del)
	if del2RR.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d", del2RR.Code)
	}
}

func TestShareFullFromRelBoundary(t *testing.T) {
	s, fileDir := newShareService(t)
	cases := []struct {
		rel  string
		want bool
	}{
		{"a.txt", true},
		{"sub/a.txt", true},
		{"", false},
		{"../secret", false},
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		full, ok := s.shareFullFromRel(c.rel)
		if ok != c.want {
			t.Errorf("shareFullFromRel(%q) ok = %v, want %v", c.rel, ok, c.want)
			continue
		}
		if ok && !(full == fileDir || strings.HasPrefix(full, fileDir+string(os.PathSeparator))) {
			t.Errorf("shareFullFromRel(%q) = %q 越界", c.rel, full)
		}
	}
}
