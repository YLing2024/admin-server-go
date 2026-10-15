package notifications

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"admin-server-go/internal/config"
	"admin-server-go/internal/sqlite"
	"admin-server-go/internal/store"
)

// PNG_BYTES 是 1x1 PNG 头字节，仅用于构造合法 base64 负载（与旧单测一致）。
var pngBytes = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// newTestService 打开一个临时 SQLite 通知库并构造 Service。
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	dialect := sqlite.New()
	ctx := context.Background()
	db, err := store.Open(ctx, dialect, sqlite.DSN(filepath.Join(dir, "notifications.db")), 1, dialect.MigrateNotifications)
	if err != nil {
		t.Fatalf("打开测试通知库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{
		NotificationFilesDir:         filepath.Join(dir, "notification-files"),
		NotificationRetentionSeconds: 180 * 24 * 3600,
		NotificationRetentionDays:    180,
		AuditLog:                     filepath.Join(dir, "audit.log"),
	}
	return New(Options{Cfg: cfg, DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func ptr(s string) *string { return &s }

// testWrite 用默认参数写入一条通知，overrides 覆盖字段。
func testWrite(t *testing.T, s *Service, in writeInput) writeResult {
	t.Helper()
	if in.source == "" {
		in.source = "cron"
	}
	if in.typeKey == "" {
		in.typeKey = "cron"
	}
	if in.title == "" {
		in.title = "测试通知"
	}
	res, err := s.writeNotification(context.Background(), in)
	if err != nil {
		t.Fatalf("写入通知失败: %v", err)
	}
	return res
}

func TestSanitizeAttachmentName(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":       "passwd",
		"a/b/c.png":              "c.png",
		`..\..\windows\x.txt`:    "x.txt",
		".hidden":                "hidden",
		"报告 1.png":               "___1.png",
		strings.Repeat("x", 100): strings.Repeat("x", 80),
		"":                       "file",
		"/":                      "file",
		"a/":                     "file",
		"photo.WEBP":             "photo.WEBP",
	}
	for in, want := range cases {
		if got := sanitizeAttachmentName(in); got != want {
			t.Errorf("sanitizeAttachmentName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseAttachments(t *testing.T) {
	bad := parseAttachments([]any{map[string]any{"name": "a.zip", "mime": "application/zip", "dataBase64": b64(pngBytes)}})
	if bad.ok || bad.status != 400 {
		t.Fatalf("白名单外 mime 应 400，得到 ok=%v status=%d", bad.ok, bad.status)
	}

	five := make([]any, 5)
	for i := range five {
		five[i] = map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": b64(pngBytes)}
	}
	if got := parseAttachments(five); got.ok || got.status != 413 {
		t.Fatalf("超过数量上限应 413，得到 ok=%v status=%d", got.ok, got.status)
	}

	huge := bytes.Repeat([]byte{1}, attachmentMaxFileBytes+1)
	if got := parseAttachments([]any{map[string]any{"name": "big.png", "mime": "image/png", "dataBase64": b64(huge)}}); got.ok || got.status != 413 {
		t.Fatalf("单文件超限应 413，得到 ok=%v status=%d", got.ok, got.status)
	}

	over := bytes.Repeat([]byte{1}, 7*1024*1024)
	total := []any{
		map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": b64(over)},
		map[string]any{"name": "b.png", "mime": "image/png", "dataBase64": b64(over)},
		map[string]any{"name": "c.png", "mime": "image/png", "dataBase64": b64(over)},
	}
	if got := parseAttachments(total); got.ok || got.status != 413 {
		t.Fatalf("合计超限应 413，得到 ok=%v status=%d", got.ok, got.status)
	}

	if got := parseAttachments(map[string]any{}); got.ok || got.status != 400 {
		t.Fatalf("非数组应 400，得到 ok=%v status=%d", got.ok, got.status)
	}
	if got := parseAttachments(nil); !got.ok || len(got.items) != 0 {
		t.Fatalf("缺省应为 ok 空列表，得到 %+v", got)
	}
	if got := parseAttachments([]any{map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": ""}}); got.ok || got.status != 400 {
		t.Fatalf("空数据应 400，得到 ok=%v status=%d", got.ok, got.status)
	}
}

func TestParseAttachmentsDedupeName(t *testing.T) {
	got := parseAttachments([]any{
		map[string]any{"name": "report.png", "mime": "image/png", "dataBase64": b64(pngBytes)},
		map[string]any{"name": "report.png", "mime": "image/png", "dataBase64": b64(pngBytes)},
	})
	if !got.ok {
		t.Fatalf("应解析成功: %+v", got)
	}
	if names := []string{got.items[0].name, got.items[1].name}; names[0] != "report.png" || names[1] != "report-2.png" {
		t.Fatalf("重名去重失败: %v", names)
	}
}

func TestParseAttachmentsDataURLPrefixAndLenientBase64(t *testing.T) {
	got := parseAttachments([]any{
		map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": "data:image/png;base64," + b64(pngBytes)},
	})
	if !got.ok || !bytes.Equal(got.items[0].data, pngBytes) {
		t.Fatalf("data URL 前缀应被剥离且解码正确: %+v", got)
	}
	// 含空白/换行的宽松解码应对齐 Node 行为
	spaced := b64(pngBytes)
	got2 := parseAttachments([]any{map[string]any{"name": "b.png", "mime": "image/png", "dataBase64": spaced[:4] + "\n" + spaced[4:]}})
	if !got2.ok || !bytes.Equal(got2.items[0].data, pngBytes) {
		t.Fatalf("含换行的 base64 应被宽松解码: %+v", got2)
	}
}

func TestSaveNotificationAttachmentsModesAndMeta(t *testing.T) {
	s := newTestService(t)
	res := testWrite(t, s, writeInput{title: "带附件"})
	parsed := parseAttachments([]any{
		map[string]any{"name": "shot.png", "mime": "image/png", "dataBase64": b64(pngBytes)},
		map[string]any{"name": "notes.md", "mime": "text/markdown", "dataBase64": b64([]byte("# hi"))},
	})
	metas, warnings := s.saveNotificationAttachments(context.Background(), res.ID, parsed.items)
	if len(warnings) != 0 || len(metas) != 2 {
		t.Fatalf("落盘应无 warning 且 2 个附件: metas=%v warnings=%v", metas, warnings)
	}
	dir := s.attachmentDir(res.ID)
	if info, err := statMode(dir); err != nil || info != 0o700 {
		t.Fatalf("附件目录权限应为 0700，得到 %o err=%v", info, err)
	}
	if info, err := statMode(filepath.Join(dir, "shot.png")); err != nil || info != 0o600 {
		t.Fatalf("附件文件权限应为 0600，得到 %o err=%v", info, err)
	}
	list, err := s.listAttachments(context.Background(), res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "notes.md" || list[1].Name != "shot.png" {
		t.Fatalf("元信息应按 name 升序: %v", list)
	}
}

func TestSaveNotificationAttachmentsOverwrite(t *testing.T) {
	s := newTestService(t)
	res := testWrite(t, s, writeInput{title: "覆盖"})
	first := parseAttachments([]any{map[string]any{"name": "old.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(context.Background(), res.ID, first.items)
	second := parseAttachments([]any{map[string]any{"name": "new.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(context.Background(), res.ID, second.items)
	list, _ := s.listAttachments(context.Background(), res.ID)
	if len(list) != 1 || list[0].Name != "new.png" {
		t.Fatalf("覆盖式写入应只剩新附件: %v", list)
	}
}

func TestAttachmentDownloadHeaders(t *testing.T) {
	html := attachmentDownloadHeaders("text/html")
	if html["Content-Security-Policy"] != "sandbox" || html["X-Content-Type-Options"] != "nosniff" {
		t.Fatalf("text/html 应下发 CSP sandbox + nosniff: %v", html)
	}
	if strings.Contains(html["Content-Security-Policy"], "allow-scripts") || strings.Contains(html["Content-Security-Policy"], "allow-same-origin") {
		t.Fatalf("CSP 不得放开脚本或同源身份: %v", html)
	}
	if got := attachmentDownloadHeaders("image/png"); len(got) != 0 {
		t.Fatalf("其它 mime 不应加头: %v", got)
	}
}

func TestListAndGetWithAttachments(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	res := testWrite(t, s, writeInput{title: "列表附件"})
	parsed := parseAttachments([]any{map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(ctx, res.ID, parsed.items)

	list, err := s.listNotifications(ctx, listFilters{limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("应有一条通知: %v", list.Items)
	}
	item := list.Items[0]
	if len(item.Attachments) != 1 || !item.HasImage {
		t.Fatalf("列表应带附件与图片标记: %+v", item)
	}
	if item.Attachments[0].URL != "/api/admin/notifications/"+strconv.FormatInt(res.ID, 10)+"/files/a.png" {
		t.Fatalf("下载地址不正确: %s", item.Attachments[0].URL)
	}

	detail, ok, err := s.getNotification(ctx, res.ID)
	if err != nil || !ok || len(detail.Attachments) != 1 || !detail.HasImage {
		t.Fatalf("详情应带附件: ok=%v err=%v detail=%+v", ok, err, detail)
	}
	if _, ok, _ := s.getNotification(ctx, 999999); ok {
		t.Fatal("不存在的 id 应未命中")
	}
}

func TestFindAttachmentTraversal(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	res := testWrite(t, s, writeInput{title: "下载"})
	parsed := parseAttachments([]any{map[string]any{"name": "doc.pdf", "mime": "application/pdf", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(ctx, res.ID, parsed.items)

	found, err := s.findAttachment(ctx, res.ID, "doc.pdf")
	if err != nil || found == nil || found.mime != "application/pdf" {
		t.Fatalf("应命中 doc.pdf: found=%+v err=%v", found, err)
	}
	if got, _ := s.findAttachment(ctx, res.ID, "../doc.pdf"); got == nil || got.name != "doc.pdf" {
		t.Fatalf("basename 归一后应命中: %+v", got)
	}
	for _, name := range []string{"../../etc/passwd", "missing.pdf", "", ".."} {
		if got, _ := s.findAttachment(ctx, res.ID, name); got != nil {
			t.Fatalf("非法/未命中 name=%q 应为 nil，得到 %+v", name, got)
		}
	}
}

func TestDeleteNotificationRemovesAttachments(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	res := testWrite(t, s, writeInput{title: "待删"})
	parsed := parseAttachments([]any{map[string]any{"name": "x.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(ctx, res.ID, parsed.items)
	dir := s.attachmentDir(res.ID)

	if err := s.deleteNotification(ctx, res.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := statMode(dir); err == nil {
		t.Fatal("附件目录应被删除")
	}
	if _, ok, _ := s.getNotification(ctx, res.ID); ok {
		t.Fatal("通知应被删除")
	}
	if list, _ := s.listAttachments(ctx, res.ID); len(list) != 0 {
		t.Fatalf("附件元信息应清空: %v", list)
	}
}

func TestCleanupExpiredRemovesRowAndAttachments(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	res := testWrite(t, s, writeInput{title: "过期"})
	parsed := parseAttachments([]any{map[string]any{"name": "old.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(ctx, res.ID, parsed.items)
	dir := s.attachmentDir(res.ID)
	if _, err := s.db.SQL.ExecContext(ctx, "UPDATE notifications SET ts = ? WHERE id = ?", nowSeconds()-200*86400, res.ID); err != nil {
		t.Fatal(err)
	}

	s.cleanupExpired()

	if _, ok, _ := s.getNotification(ctx, res.ID); ok {
		t.Fatal("过期通知应被清理")
	}
	if _, err := statMode(dir); err == nil {
		t.Fatal("过期附件目录应被清理")
	}
	if list, _ := s.listAttachments(ctx, res.ID); len(list) != 0 {
		t.Fatalf("过期附件元信息应清空: %v", list)
	}
}

func TestBulkDeleteDryRunAndReal(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	a := testWrite(t, s, writeInput{title: "批量A", typeKey: "bulk-a", source: "bulk-a"})
	b := testWrite(t, s, writeInput{title: "批量B", typeKey: "bulk-b", source: "bulk-b"})
	parsed := parseAttachments([]any{map[string]any{"name": "p.png", "mime": "image/png", "dataBase64": b64(pngBytes)}})
	s.saveNotificationAttachments(ctx, a.ID, parsed.items)

	srcA := "bulk-a"
	dry, err := s.bulkDeleteNotifications(ctx, bulkDeleteFilters{source: &srcA}, true)
	if err != nil || dry != 1 {
		t.Fatalf("dryRun 应统计 1，得到 %d err=%v", dry, err)
	}
	if _, ok, _ := s.getNotification(ctx, a.ID); !ok {
		t.Fatal("dryRun 不应删除")
	}
	deleted, err := s.bulkDeleteNotifications(ctx, bulkDeleteFilters{source: &srcA}, false)
	if err != nil || deleted != 1 {
		t.Fatalf("实删应为 1，得到 %d err=%v", deleted, err)
	}
	if _, ok, _ := s.getNotification(ctx, a.ID); ok {
		t.Fatal("实删应删除 A")
	}
	if _, err := statMode(s.attachmentDir(a.ID)); err == nil {
		t.Fatal("实删应清理 A 的附件目录")
	}
	if _, ok, _ := s.getNotification(ctx, b.ID); !ok {
		t.Fatal("其它来源不应受影响")
	}
}

func TestDedupSameKeyWithinWindow(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	first := testWrite(t, s, writeInput{title: "第一次", dedupKey: ptr("k1"), body: ptr("v1")})
	if first.Deduped {
		t.Fatal("首次写入不应去重")
	}
	if err := s.markNotificationRead(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second := testWrite(t, s, writeInput{title: "第二次", dedupKey: ptr("k1"), body: ptr("v2")})
	if !second.Deduped || second.ID != first.ID {
		t.Fatalf("同 key 窗口内应去重更新同一行: %+v", second)
	}
	detail, _, _ := s.getNotification(ctx, first.ID)
	if detail.Title != "第二次" || detail.Body == nil || *detail.Body != "v2" || detail.ReadAt != nil {
		t.Fatalf("去重应更新内容并置未读: %+v", detail)
	}
	total, _ := s.count(ctx, "SELECT COUNT(*) FROM notifications")
	if total != 1 {
		t.Fatalf("去重后应只有一行，得到 %d", total)
	}
}

func TestDedupOutsideWindowCreatesNewRow(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	first := testWrite(t, s, writeInput{title: "旧", dedupKey: ptr("k2")})
	if _, err := s.db.SQL.ExecContext(ctx, "UPDATE notifications SET ts = ? WHERE id = ?", nowSeconds()-700, first.ID); err != nil {
		t.Fatal(err)
	}
	second := testWrite(t, s, writeInput{title: "新", dedupKey: ptr("k2")})
	if second.Deduped || second.ID == first.ID {
		t.Fatalf("窗口外应新增一行: %+v", second)
	}
	total, _ := s.count(ctx, "SELECT COUNT(*) FROM notifications")
	if total != 2 {
		t.Fatalf("窗口外应有两行，得到 %d", total)
	}
}

func TestDefaultLevelAndAutoRegisterType(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	// watchdog 预置类别默认 urgent
	res := testWrite(t, s, writeInput{title: "看门狗", typeKey: "watchdog", source: "watchdog", levelInput: ""})
	if res.Level != "urgent" {
		t.Fatalf("watchdog 默认级别应为 urgent，得到 %s", res.Level)
	}
	// 未注册类别自动注册，且默认 normal
	res2 := testWrite(t, s, writeInput{title: "新类别", typeKey: "brand-new", source: "brand-new", levelInput: ""})
	if res2.Level != "normal" {
		t.Fatalf("未注册类别默认应为 normal，得到 %s", res2.Level)
	}
	var label string
	if err := s.db.SQL.QueryRowContext(ctx, "SELECT label FROM notification_types WHERE key = ?", "brand-new").Scan(&label); err != nil || label != "brand-new" {
		t.Fatalf("未注册类别应自动注册 label=key: label=%q err=%v", label, err)
	}
}

func TestListFilters(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	testWrite(t, s, writeInput{title: "u1", typeKey: "ta", source: "sa", levelInput: "urgent"})
	testWrite(t, s, writeInput{title: "u2", typeKey: "ta", source: "sb", levelInput: "normal"})
	testWrite(t, s, writeInput{title: "u3", typeKey: "tb", source: "sa", levelInput: "normal"})

	urgent := "urgent"
	res, _ := s.listNotifications(ctx, listFilters{limit: 50, level: &urgent})
	if len(res.Items) != 1 || res.Items[0].Title != "u1" {
		t.Fatalf("level 过滤失败: %v", res.Items)
	}
	ta := "ta"
	res, _ = s.listNotifications(ctx, listFilters{limit: 50, typ: &ta})
	if len(res.Items) != 2 {
		t.Fatalf("type 过滤失败: %v", res.Items)
	}
	if res.Total != 3 || res.Unread != 3 {
		t.Fatalf("total/unread 应为全库计数: %+v", res)
	}
	// limit 与 before
	res, _ = s.listNotifications(ctx, listFilters{limit: 2})
	if len(res.Items) != 2 {
		t.Fatalf("limit 应生效: %v", res.Items)
	}
	before := res.Items[1].ID
	res, _ = s.listNotifications(ctx, listFilters{limit: 50, before: &before})
	if len(res.Items) != 1 || res.Items[0].ID >= before {
		t.Fatalf("before 应只返回更早的行: %v", res.Items)
	}
}

func TestTypesListAndPatch(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	testWrite(t, s, writeInput{title: "cron 通知", typeKey: "cron", source: "cron"})

	types, err := s.listNotificationTypes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ty := range types.Types {
		if ty.Key == "cron" {
			found = true
			if ty.Count != 1 || ty.Unread != 1 {
				t.Fatalf("类别计数不正确: %+v", ty)
			}
		}
	}
	if !found {
		t.Fatal("预置类别 cron 应存在")
	}

	var patch typePatch
	patch.set("label", "定时播报改")
	if n, err := s.updateNotificationType(ctx, "cron", patch); err != nil || n != 1 {
		t.Fatalf("更新现有关键字应返回 1: n=%d err=%v", n, err)
	}
	// 同值再更新（no-op）仍应视为命中，避免 MySQL 报「类别不存在」
	if n, err := s.updateNotificationType(ctx, "cron", patch); err != nil || n != 1 {
		t.Fatalf("no-op 更新应仍视为命中: n=%d err=%v", n, err)
	}
	if n, err := s.updateNotificationType(ctx, "no-such-key", patch); err != nil || n != 0 {
		t.Fatalf("不存在的类别应返回 0: n=%d err=%v", n, err)
	}
}

// statMode 返回路径的权限位；不存在返回错误。
func statMode(path string) (uint32, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return uint32(info.Mode().Perm()), nil
}
