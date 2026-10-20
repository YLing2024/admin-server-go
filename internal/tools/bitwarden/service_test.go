package bitwarden

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
)

// testToken 生成仅够解析 exp 的 JWT；签名段不需要校验，本服务只读 exp。
func testToken(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]int64{"exp": exp})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

// testConfig 指向临时会话文件，避免用例碰到真实的 ~/.bw_session。
func testConfig(t *testing.T, server string) *config.Config {
	t.Helper()
	return &config.Config{
		BitwardenServer:      server,
		BitwardenEmail:       "user@example.com",
		BitwardenDeviceID:    "device-test",
		BitwardenSessionFile: filepath.Join(t.TempDir(), ".bw_session"),
	}
}

// unlockRequest 构造与路由层一致的请求：body 已由 httpx.WithBody 写入上下文。
func unlockRequest(t *testing.T, password, ip string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/admin/tools/bitwarden/unlock", nil)
	r.Header.Set("X-Real-IP", ip)
	return httpx.WithBody(r, map[string]any{"password": password})
}

// fakeBW 在临时目录放一个假的 bw 可执行文件并把它放到 PATH 最前面。
// 解锁逻辑改为委托官方 CLI（KDF 由 CLI 负责），因此测试用假 CLI 复现行为：
// 读一行密码，匹配 $FAKE_BW_PASSWORD 就打印 $FAKE_BW_TOKEN，否则退出 1。
func fakeBW(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
read -r pw
if [ "$pw" = "$FAKE_BW_PASSWORD" ]; then
  printf '? Master password: [input is hidden]\n'
  printf '%s\n' "$FAKE_BW_TOKEN"
  exit 0
fi
printf 'Cryptography error, The decryption operation failed\n'
exit 1
`
	path := filepath.Join(dir, "bw")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("写假 bw: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	// 缩短 PTY 回显等待，避免每个用例都等 700ms。
	old := cliEchoGrace
	cliEchoGrace = 20 * time.Millisecond
	t.Cleanup(func() { cliEchoGrace = old })
}

// TestUnlockWritesSessionAndStatus 覆盖成功路径：0600 文件、状态剩余时间、清单与清除。
func TestUnlockWritesSessionAndStatus(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token := testToken(now.Unix() + 3600)
	fakeBW(t)
	t.Setenv("FAKE_BW_PASSWORD", "s3cret")
	t.Setenv("FAKE_BW_TOKEN", token)

	cfg := testConfig(t, "http://example.invalid")
	svc := New(Options{Cfg: cfg, Now: func() time.Time { return now }})
	rec := httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest(t, "s3cret", "198.51.100.7"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("响应不得包含密码或 token: %s", rec.Body.String())
	}

	info, err := os.Stat(cfg.BitwardenSessionFile)
	if err != nil {
		t.Fatalf("会话文件不存在: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("会话文件权限 = %o, want 600", mode)
	}
	raw, err := os.ReadFile(cfg.BitwardenSessionFile)
	if err != nil {
		t.Fatalf("读取会话文件: %v", err)
	}
	if string(raw) != token {
		t.Fatalf("会话文件内容 = %q, want token", string(raw))
	}

	rec = httptest.NewRecorder()
	svc.Status(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析状态: %v", err)
	}
	if !status.Unlocked || status.ExpiresInSeconds == nil || *status.ExpiresInSeconds != 3600 {
		t.Fatalf("状态 = %+v, want unlocked 且剩余 3600 秒", status)
	}
	if status.UpdatedAt == nil || *status.UpdatedAt == 0 {
		t.Fatalf("updatedAt 缺失: %+v", status)
	}

	rec = httptest.NewRecorder()
	svc.List(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var tools []Tool
	if err := json.Unmarshal(rec.Body.Bytes(), &tools); err != nil {
		t.Fatalf("解析工具清单: %v", err)
	}
	if len(tools) != 1 || tools[0].ID != "bitwarden" || !tools[0].Configured || !tools[0].Unlocked {
		t.Fatalf("工具清单 = %+v", tools)
	}

	rec = httptest.NewRecorder()
	svc.Lock(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("lock status = %d", rec.Code)
	}
	if _, err := os.Stat(cfg.BitwardenSessionFile); !os.IsNotExist(err) {
		t.Fatalf("lock 后会话文件应消失, err = %v", err)
	}
}

// TestUnlockFailureIsOpaque 校验 CLI 失败统一回 401「密码错误」，且不落文件。
func TestUnlockFailureIsOpaque(t *testing.T) {
	fakeBW(t)
	t.Setenv("FAKE_BW_PASSWORD", "correct-one")
	t.Setenv("FAKE_BW_TOKEN", "irrelevant-token-value-that-is-long-enough-to-parse-1234567890")

	cfg := testConfig(t, "http://example.invalid")
	svc := New(Options{Cfg: cfg})
	rec := httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest(t, "wrong-password-value", "198.51.100.8"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.OK || body.Error != "密码错误" {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "wrong-password-value") ||
		strings.Contains(rec.Body.String(), "Cryptography error") {
		t.Fatalf("响应不得泄漏密码或 CLI 细节: %s", rec.Body.String())
	}
	if _, err := os.Stat(cfg.BitwardenSessionFile); !os.IsNotExist(err) {
		t.Fatalf("失败不应写会话文件, err = %v", err)
	}
}

// TestUnlockRateLimit 校验同 IP 窗口内第 6 次返回 429（前 5 次失败不提前限流）。
func TestUnlockRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fakeBW(t)
	t.Setenv("FAKE_BW_PASSWORD", "correct-one")
	t.Setenv("FAKE_BW_TOKEN", "irrelevant-token-value-that-is-long-enough-to-parse-1234567890")

	svc := New(Options{Cfg: testConfig(t, "http://example.invalid"), Now: func() time.Time { return now }})
	for i := 1; i <= 5; i++ {
		rec := httptest.NewRecorder()
		svc.Unlock(rec, unlockRequest(t, "bad", "203.0.113.9"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次 status = %d, want 401", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest(t, "bad", "203.0.113.9"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次 status = %d, want 429", rec.Code)
	}

	// 另一个 IP 不受影响，说明桶按客户端隔离。
	rec = httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest(t, "bad", "203.0.113.10"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("其它 IP status = %d, want 401", rec.Code)
	}
}

// TestExtractSessionToken 校验从混有提示符与 ANSI 的 PTY 输出里取出裸令牌。
func TestExtractSessionToken(t *testing.T) {
	token := strings.Repeat("Ab3+/", 12) + "=="
	out := "? Master password: [input is hidden]\r\n" +
		"\x1b[?25l\x1b[2K" + token + "\x1b[?25h\r\n"
	if got := extractSessionToken(out); got != token {
		t.Fatalf("extractSessionToken = %q, want %q", got, token)
	}
	if got := extractSessionToken("Master password: [hidden]\nERROR: nope\n"); got != "" {
		t.Fatalf("无令牌时应返回空, got %q", got)
	}
}

// TestStripANSI 校验转义序列被清掉，且普通文本原样保留。
func TestStripANSI(t *testing.T) {
	if got := stripANSI("\x1b[2Kabc\x1b[0m"); got != "abc" {
		t.Fatalf("stripANSI = %q", got)
	}
	if got := stripANSI("plain-text-123"); got != "plain-text-123" {
		t.Fatalf("stripANSI 破坏了普通文本: %q", got)
	}
}

// TestStatusNonJWTIsUnlockedWithoutExpiry 校验非 JWT 按「已解锁、有效期未知」处理。
func TestStatusNonJWTIsUnlockedWithoutExpiry(t *testing.T) {
	cfg := testConfig(t, "http://example.invalid")
	if err := os.WriteFile(cfg.BitwardenSessionFile, []byte("opaque-session-token"), 0o600); err != nil {
		t.Fatalf("写会话文件: %v", err)
	}
	svc := New(Options{Cfg: cfg})
	rec := httptest.NewRecorder()
	svc.Status(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析状态: %v", err)
	}
	if !status.Unlocked || status.ExpiresInSeconds != nil {
		t.Fatalf("状态 = %+v, want unlocked 且 expiresInSeconds=null", status)
	}
}

// TestExpiredJWTIsLocked 校验过期 token 仍占用文件时对外按未解锁呈现。
func TestExpiredJWTIsLocked(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfg := testConfig(t, "http://example.invalid")
	token := testToken(now.Unix() - 1)
	if err := os.WriteFile(cfg.BitwardenSessionFile, []byte(token), 0o600); err != nil {
		t.Fatalf("写会话文件: %v", err)
	}
	svc := New(Options{Cfg: cfg, Now: func() time.Time { return now }})
	rec := httptest.NewRecorder()
	svc.Status(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var status Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析状态: %v", err)
	}
	if status.Unlocked {
		t.Fatalf("过期 token 应视为未解锁: %+v", status)
	}
}

// TestUnconfiguredReturns501 校验三项必填缺失时接口 501，清单仍可用且标记未配置。
func TestUnconfiguredReturns501(t *testing.T) {
	svc := New(Options{Cfg: &config.Config{BitwardenSessionFile: filepath.Join(t.TempDir(), ".bw_session")}})

	rec := httptest.NewRecorder()
	svc.Status(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status 未配置 = %d, want 501", rec.Code)
	}
	rec = httptest.NewRecorder()
	svc.Unlock(rec, unlockRequest(t, "x", "198.51.100.11"))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unlock 未配置 = %d, want 501", rec.Code)
	}
	rec = httptest.NewRecorder()
	svc.Lock(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("lock 未配置 = %d, want 501", rec.Code)
	}

	rec = httptest.NewRecorder()
	svc.List(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var tools []Tool
	if err := json.Unmarshal(rec.Body.Bytes(), &tools); err != nil {
		t.Fatalf("解析工具清单: %v", err)
	}
	if len(tools) != 1 || tools[0].Configured || tools[0].Unlocked {
		t.Fatalf("未配置时清单 = %+v", tools)
	}
}
