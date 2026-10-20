package sshkeys

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	drivermysql "github.com/go-sql-driver/mysql"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	sqlmysql "admin-server-go/internal/mysql"
	"admin-server-go/internal/store"
	"admin-server-go/internal/testsupport"
)

// newTestService 建一个带 ssh_key 表的临时库并返回服务。
func newTestService(t *testing.T) (*Service, *store.DB, string) {
	t.Helper()
	dsn := testsupport.SchemaDSN(t, "sshkeys")
	db, err := store.Open(context.Background(), sqlmysql.New(), dsn, 4, func(ctx context.Context, conn *sql.DB) error {
		return sqlmysql.New().MigrateSSHKeys(ctx, conn)
	})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{AuditLog: t.TempDir() + "/audit.log"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Options{Cfg: cfg, DB: db, Log: logger}), db, dsn
}

// call 直接调用域处理器，跳过鉴权（鉴权由 httpapi 的路由包装负责）。
func call(t *testing.T, handler http.HandlerFunc, method, path string, body map[string]any, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req = httpx.WithBody(req, body)
	}
	if params != nil {
		req = httpx.WithParams(req, params)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeView(t *testing.T, rec *httptest.ResponseRecorder) keyView {
	t.Helper()
	var view keyView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析响应失败: %v / %s", err, rec.Body.String())
	}
	return view
}

// TestServiceCRUD 覆盖新增、列表、重复 409、带选项 400、改标题/停用、删除。
func TestServiceCRUD(t *testing.T) {
	svc, _, _ := newTestService(t)

	rec := call(t, svc.Create, http.MethodPost, "/api/admin/ssh-keys",
		map[string]any{"title": "办公笔记本", "public_key": testEd25519}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("新增状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	created := decodeView(t, rec)
	if created.ID == "" || created.Fingerprint != "SHA256:9zCYsPfTdI1YOcwKruw2isdw+HCf6Z6w3kxtEm2lZLI" || !created.Enabled {
		t.Fatalf("新增响应不符: %+v", created)
	}

	rec = call(t, svc.List, http.MethodGet, "/api/admin/ssh-keys", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d", rec.Code)
	}
	var list struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Keys) != 1 {
		t.Fatalf("列表解析失败: %v / %s", err, rec.Body.String())
	}

	rec = call(t, svc.Create, http.MethodPost, "/api/admin/ssh-keys",
		map[string]any{"title": "重复", "public_key": testEd25519}, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复公钥状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, svc.Create, http.MethodPost, "/api/admin/ssh-keys",
		map[string]any{"title": "注入", "public_key": `command="id" ` + testEd25519}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带前置选项状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = call(t, svc.Update, http.MethodPatch, "/api/admin/ssh-keys/"+created.ID,
		map[string]any{"title": "备用机", "enabled": false}, map[string]string{"id": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("更新状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	updated := decodeView(t, rec)
	if updated.Title != "备用机" || updated.Enabled {
		t.Fatalf("更新结果不符: %+v", updated)
	}

	rec = call(t, svc.Delete, http.MethodDelete, "/api/admin/ssh-keys/"+created.ID, nil, map[string]string{"id": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec = call(t, svc.Delete, http.MethodDelete, "/api/admin/ssh-keys/"+created.ID, nil, map[string]string{"id": created.ID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除状态码 = %d", rec.Code)
	}
}

// TestServiceLimit 守住 20 把上限：第 21 把返回 400。
func TestServiceLimit(t *testing.T) {
	svc, _, _ := newTestService(t)
	for i := 0; i < maxKeys; i++ {
		line := syntheticEd25519(byte(i + 1))
		parsed, err := ParsePublicKey(line)
		if err != nil {
			t.Fatalf("构造公钥失败: %v", err)
		}
		if _, err := svc.repo.Create(context.Background(), "key-"+strconv.Itoa(i), parsed); err != nil {
			t.Fatalf("预置第 %d 把失败: %v", i+1, err)
		}
	}
	rec := call(t, svc.Create, http.MethodPost, "/api/admin/ssh-keys",
		map[string]any{"title": "第 21 把", "public_key": syntheticEd25519(200)}, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "20") {
		t.Fatalf("超限状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestServiceUnavailable 守住启动时指标库不可用的降级语义：503 而不是 500/panic。
func TestServiceUnavailable(t *testing.T) {
	svc := New(Options{Cfg: &config.Config{AuditLog: t.TempDir() + "/audit.log"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rec := call(t, svc.List, http.MethodGet, "/api/admin/ssh-keys", nil, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// TestRunHelper 直接验证辅助命令：只输出启用项、用户不符输出空、数据库不可达也输出空。
func TestRunHelper(t *testing.T) {
	svc, db, dsn := newTestService(t)
	first, err := ParsePublicKey(testEd25519)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParsePublicKey(testRSA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.repo.Create(context.Background(), "启用", first); err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.repo.Create(context.Background(), "停用", second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.repo.Update(context.Background(), disabled.ID, "停用", false); err != nil {
		t.Fatal(err)
	}

	get := helperEnv(t, dsn)
	var out bytes.Buffer
	if err := RunHelper(context.Background(), get, "root", first.Fingerprint, &out); err != nil {
		t.Fatalf("辅助命令失败: %v", err)
	}
	if out.String() != first.Line+"\n" {
		t.Fatalf("辅助命令输出 = %q, want %q", out.String(), first.Line+"\n")
	}

	var lastUsed sql.NullString
	if err := db.SQL.QueryRowContext(context.Background(), "SELECT last_used_at FROM ssh_key WHERE id = ?", 1).Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if !lastUsed.Valid {
		t.Fatal("最近使用时间未回写")
	}

	out.Reset()
	if err := RunHelper(context.Background(), get, "other", "", &out); err != nil {
		t.Fatalf("用户不符不应报错: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("用户不符应无输出，得到 %q", out.String())
	}

	out.Reset()
	err = RunHelper(context.Background(), func(key string) string {
		if key == "DB_PORT" || key == "SSH_KEYS_DB_PORT" {
			return "1"
		}
		return get(key)
	}, "root", "", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("数据库不可达应报错且无输出: err=%v out=%q", err, out.String())
	}
}

// helperEnv 从测试 DSN 还原辅助命令需要的环境变量。
func helperEnv(t *testing.T, dsn string) func(string) string {
	t.Helper()
	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("解析测试 DSN 失败: %v", err)
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatalf("解析地址失败: %v", err)
	}
	values := map[string]string{
		"DB_HOST":         host,
		"DB_PORT":         port,
		"DB_USER":         cfg.User,
		"DB_PASSWORD":     cfg.Passwd,
		"DB_METRICS_NAME": cfg.DBName,
		"SSH_KEYS_USER":   "root",
	}
	return func(key string) string { return values[key] }
}

// syntheticEd25519 生成结构合法但内容唯一的 ed25519 公钥，用于上限测试。
func syntheticEd25519(seed byte) string {
	const alg = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(alg)+4+32)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(alg)))
	blob = append(blob, buf...)
	blob = append(blob, alg...)
	binary.BigEndian.PutUint32(buf, 32)
	blob = append(blob, buf...)
	key := make([]byte, 32)
	key[0] = seed
	blob = append(blob, key...)
	return alg + " " + base64.StdEncoding.EncodeToString(blob) + " synthetic-" + strconv.Itoa(int(seed))
}
