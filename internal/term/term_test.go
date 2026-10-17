package term

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

// fakeStore 是 TicketStore 的内存实现，让单测无需真 Redis 也能覆盖票据签发路径。
type fakeStore struct {
	data map[string]string
}

func (f *fakeStore) Get(_ context.Context, key string) (string, error) { return f.data[key], nil }

func (f *fakeStore) Set(_ context.Context, key, value string, _ int) error {
	f.data[key] = value
	return nil
}

// newTestService 构造测试用 Service：password 非空时写入对应哈希的口令文件，否则不建文件。
func newTestService(t *testing.T, mode config.TermUnlockMode, password string) (*Service, *fakeStore) {
	t.Helper()
	tmp := t.TempDir()
	pwFile := filepath.Join(tmp, "term_password")
	if password != "" {
		const salt = "testsalt"
		sum := sha256.Sum256([]byte(salt + ":" + password))
		if err := os.WriteFile(pwFile, []byte("sha256$"+salt+"$"+hex.EncodeToString(sum[:])), 0o600); err != nil {
			t.Fatalf("写口令文件: %v", err)
		}
	}
	cfg := &config.Config{
		AuditLog:   filepath.Join(tmp, "audit.log"),
		TermPwFile: pwFile,
		TermUnlock: mode,
	}
	store := &fakeStore{data: map[string]string{}}
	return New(Options{Cfg: cfg, Redis: store}), store
}

// unlockRequest 造一个带已解析请求体的 POST /unlock 请求（对齐 httpapi 的解析时机）。
func unlockRequest(password string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/admin/term/unlock", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	return httpx.WithBody(r, map[string]any{"password": password})
}

// decodeUnlock 校验成功响应的字段名/类型/状态码与成功路径完全一致。
func decodeUnlock(t *testing.T, rec *httptest.ResponseRecorder, store *fakeStore) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200；body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v；body=%s", err, rec.Body.String())
	}
	if len(resp.Ticket) != 64 {
		t.Fatalf("ticket = %q，应为 64 位 hex", resp.Ticket)
	}
	if resp.ExpiresIn != ticketTTL {
		t.Fatalf("expiresIn = %d, want %d", resp.ExpiresIn, ticketTTL)
	}
	if _, ok := store.data[ticketPrefix+util.Sha256Hex(resp.Ticket)]; !ok {
		t.Fatalf("票据未按 sha256 写入存储：%v", store.data)
	}
}

// TestUnlockOffSkipsPassword 关闭口令门时：任意/空口令均 200 带票据，连续 6 次不触发限流。
func TestUnlockOffSkipsPassword(t *testing.T) {
	svc, store := newTestService(t, config.TermUnlockOff, "")
	for i, pw := range []string{"", "wrong", "whatever", "", "x", "y"} {
		rec := httptest.NewRecorder()
		svc.Unlock(rec, unlockRequest(pw))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次不应触发限流", i+1)
		}
		decodeUnlock(t, rec, store)
	}
}

// TestUnlockPasswordModeRejects 回归：口令模式下错口令 401，连续 5 次后锁 10 分钟（第 6 次 429）。
func TestUnlockPasswordModeRejects(t *testing.T) {
	svc, _ := newTestService(t, config.TermUnlockPassword, "s3cret")
	for i := 0; i < unlockMaxFails; i++ {
		rec := httptest.NewRecorder()
		svc.Unlock(rec, unlockRequest("bad"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错口令 status = %d, want 401", i+1, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest("bad"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定后 status = %d, want 429", rec.Code)
	}
	// 锁定期间即使口令正确也不放行。
	rec = httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest("s3cret"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确口令 status = %d, want 429", rec.Code)
	}
}

// TestUnlockPasswordModeSuccess 回归：对口令 200 且响应与成功路径一致。
func TestUnlockPasswordModeSuccess(t *testing.T) {
	svc, store := newTestService(t, config.TermUnlockPassword, "s3cret")
	rec := httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest("s3cret"))
	decodeUnlock(t, rec, store)
}

// TestMode 校验探测端点：off → unlockRequired=false，password → true。
func TestMode(t *testing.T) {
	cases := []struct {
		mode config.TermUnlockMode
		want bool
	}{
		{config.TermUnlockOff, false},
		{config.TermUnlockPassword, true},
	}
	for _, c := range cases {
		svc, _ := newTestService(t, c.mode, "s3cret")
		rec := httptest.NewRecorder()
		svc.Mode(rec, httptest.NewRequest(http.MethodGet, "/api/admin/term/mode", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("mode=%q status = %d, want 200", c.mode, rec.Code)
		}
		var resp struct {
			UnlockRequired bool `json:"unlockRequired"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		if resp.UnlockRequired != c.want {
			t.Fatalf("mode=%q unlockRequired = %v, want %v", c.mode, resp.UnlockRequired, c.want)
		}
	}
}

// TestVerifyOnlyLocal 守住安全边界：关不关口令门，Verify 都只允许本机来源。
func TestVerifyOnlyLocal(t *testing.T) {
	svc, _ := newTestService(t, config.TermUnlockOff, "")

	remote := httptest.NewRequest(http.MethodGet, "/api/admin/term/verify?ticket=deadbeef", nil)
	remote.RemoteAddr = "192.0.2.1:5555"
	rec := httptest.NewRecorder()
	svc.Verify(rec, remote)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本机 Verify status = %d, want 403", rec.Code)
	}

	local := httptest.NewRequest(http.MethodGet, "/api/admin/term/verify?ticket=deadbeef", nil)
	local.RemoteAddr = "127.0.0.1:5555"
	rec = httptest.NewRecorder()
	svc.Verify(rec, local)
	// 本机但票据不存在 → 401（证明放行到了 Redis 校验，而非被来源拦截）。
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("本机无效票据 Verify status = %d, want 401", rec.Code)
	}
}
