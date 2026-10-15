package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
)

// call 构造带已解析 body / 路径参数的请求并直接调用 handler，返回响应。
func call(t *testing.T, handler func(http.ResponseWriter, *http.Request), method, target string, body map[string]any, params map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
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

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %s err=%v", rec.Body.String(), err)
	}
	return out
}

func TestHandlersNilDBDegrade(t *testing.T) {
	svc := New(Options{Cfg: &config.Config{NotificationFilesDir: t.TempDir(), NotificationRetentionSeconds: 180 * 86400}})
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"Create":     svc.Create,
		"List":       svc.List,
		"TypesList":  svc.TypesList,
		"TypesPatch": svc.TypesPatch,
		"Read":       svc.Read,
		"ReadAll":    svc.ReadAll,
		"Delete":     svc.Delete,
		"BulkDelete": svc.BulkDelete,
		"Stats":      svc.Stats,
		"Get":        svc.Get,
		"Attachment": svc.Attachment,
	}
	for name, h := range handlers {
		rec := call(t, h, http.MethodPost, "/api/admin/notifications", map[string]any{}, map[string]string{"id": "1", "name": "a.png", "key": "cron"})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 在库不可用时应 503，得到 %d", name, rec.Code)
		}
		if decodeJSON(t, rec)["error"] != "通知库暂不可用" {
			t.Fatalf("%s 错误文案不符: %s", name, rec.Body.String())
		}
	}
}

func TestCreateHandlerValidationAndSuccess(t *testing.T) {
	s := newTestService(t)

	rec := call(t, s.Create, http.MethodPost, "/api/admin/notifications", map[string]any{"source": "x", "title": "t", "level": "bad"}, nil)
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "level 必须是 urgent / normal / digest" {
		t.Fatalf("非法 level 应 400: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.Create, http.MethodPost, "/api/admin/notifications", map[string]any{"source": "", "title": "t"}, nil)
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "source 与 title 不能为空" {
		t.Fatalf("空 source 应 400: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.Create, http.MethodPost, "/api/admin/notifications",
		map[string]any{"source": "cron", "title": "标题", "body": "正文", "type": "cron",
			"attachments": []any{map[string]any{"name": "a.png", "mime": "image/png", "dataBase64": b64(pngBytes)}}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("合法写入应 201: %d %s", rec.Code, rec.Body.String())
	}
	out := decodeJSON(t, rec)
	if out["id"] == nil || out["attachments"] == nil {
		t.Fatalf("应返回 id 与 attachments: %s", rec.Body.String())
	}
	att := out["attachments"].([]any)[0].(map[string]any)
	if att["mime"] != "image/png" || att["url"] == nil {
		t.Fatalf("附件视图不正确: %v", att)
	}

	// 非法附件不应产生通知
	before, _ := s.count(context.Background(), "SELECT COUNT(*) FROM notifications")
	rec = call(t, s.Create, http.MethodPost, "/api/admin/notifications",
		map[string]any{"source": "cron", "title": "标题", "attachments": []any{map[string]any{"name": "a.zip", "mime": "application/zip", "dataBase64": b64(pngBytes)}}}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法附件应 400: %d %s", rec.Code, rec.Body.String())
	}
	after, _ := s.count(context.Background(), "SELECT COUNT(*) FROM notifications")
	if before != after {
		t.Fatal("非法附件请求不应写入通知")
	}
}

func TestListGetHandlers(t *testing.T) {
	s := newTestService(t)
	res := testWrite(t, s, writeInput{title: "列表", typeKey: "cron", source: "cron"})

	rec := call(t, s.List, http.MethodGet, "/api/admin/notifications?limit=1&unread=1", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200: %d", rec.Code)
	}
	out := decodeJSON(t, rec)
	if out["items"] == nil || out["total"] == nil {
		t.Fatalf("列表响应字段不全: %s", rec.Body.String())
	}

	rec = call(t, s.Get, http.MethodGet, "/api/admin/notifications/"+strconvFormat(res.ID), nil, map[string]string{"id": strconvFormat(res.ID)})
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["item"] == nil {
		t.Fatalf("详情应 200 带 item: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.Get, http.MethodGet, "/api/admin/notifications/999999", nil, map[string]string{"id": "999999"})
	if rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "通知不存在" {
		t.Fatalf("不存在应 404: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.Get, http.MethodGet, "/api/admin/notifications/abc", nil, map[string]string{"id": "abc"})
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "无效的 id" {
		t.Fatalf("非法 id 应 400: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadReadAllDeleteHandlers(t *testing.T) {
	s := newTestService(t)
	a := testWrite(t, s, writeInput{title: "a"})
	testWrite(t, s, writeInput{title: "b", typeKey: "other", source: "other"})

	rec := call(t, s.Read, http.MethodPost, "/api/admin/notifications/"+strconvFormat(a.ID)+"/read", map[string]any{}, map[string]string{"id": strconvFormat(a.ID)})
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["ok"] != true {
		t.Fatalf("单条已读应 200 ok: %d %s", rec.Code, rec.Body.String())
	}
	detail, _, _ := s.getNotification(context.Background(), a.ID)
	if detail.ReadAt == nil {
		t.Fatal("单条已读应置 readAt")
	}

	rec = call(t, s.ReadAll, http.MethodPost, "/api/admin/notifications/read-all", map[string]any{}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("全部已读应 200: %d", rec.Code)
	}
	if decodeJSON(t, rec)["count"].(float64) != 1 {
		t.Fatalf("全部已读应影响 1 条（a 已读）: %s", rec.Body.String())
	}

	rec = call(t, s.Delete, http.MethodDelete, "/api/admin/notifications/"+strconvFormat(a.ID), map[string]any{}, map[string]string{"id": strconvFormat(a.ID)})
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["ok"] != true {
		t.Fatalf("删除应 200 ok: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := s.getNotification(context.Background(), a.ID); ok {
		t.Fatal("删除后应查不到")
	}
}

func TestBulkDeleteHandler(t *testing.T) {
	s := newTestService(t)
	testWrite(t, s, writeInput{title: "a", typeKey: "ba", source: "ba"})
	testWrite(t, s, writeInput{title: "b", typeKey: "ba", source: "ba"})

	// 未读与已读同时给出 → 不匹配任何行
	rec := call(t, s.BulkDelete, http.MethodPost, "/api/admin/notifications/bulk-delete",
		map[string]any{"unreadOnly": true, "readOnly": true}, nil)
	if decodeJSON(t, rec)["count"].(float64) != 0 {
		t.Fatalf("互斥筛选应 0: %s", rec.Body.String())
	}

	rec = call(t, s.BulkDelete, http.MethodPost, "/api/admin/notifications/bulk-delete",
		map[string]any{"source": "ba", "dryRun": true}, nil)
	if decodeJSON(t, rec)["count"].(float64) != 2 {
		t.Fatalf("dryRun 应统计 2: %s", rec.Body.String())
	}
	rec = call(t, s.BulkDelete, http.MethodPost, "/api/admin/notifications/bulk-delete",
		map[string]any{"source": "ba"}, nil)
	if decodeJSON(t, rec)["count"].(float64) != 2 {
		t.Fatalf("实删应 2: %s", rec.Body.String())
	}
	total, _ := s.count(context.Background(), "SELECT COUNT(*) FROM notifications")
	if total != 0 {
		t.Fatalf("应全部删除，剩 %d", total)
	}
}

func TestStatsHandler(t *testing.T) {
	s := newTestService(t)
	testWrite(t, s, writeInput{title: "1", typeKey: "sa", source: "sa"})
	testWrite(t, s, writeInput{title: "2", typeKey: "sb", source: "sb"})
	rec := call(t, s.Stats, http.MethodGet, "/api/admin/notifications/stats", nil, nil)
	out := decodeJSON(t, rec)
	if out["total"].(float64) != 2 || out["unread"].(float64) != 2 || len(out["sources"].([]any)) != 2 {
		t.Fatalf("统计不正确: %s", rec.Body.String())
	}
}

func TestTypesHandlers(t *testing.T) {
	s := newTestService(t)
	rec := call(t, s.TypesList, http.MethodGet, "/api/admin/notifications/types", nil, nil)
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["types"] == nil {
		t.Fatalf("类别列表应 200: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.TypesPatch, http.MethodPatch, "/api/admin/notifications/types/cron", map[string]any{"label": " "}, map[string]string{"key": "cron"})
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "label 不能为空" {
		t.Fatalf("空 label 应 400: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, s.TypesPatch, http.MethodPatch, "/api/admin/notifications/types/cron", map[string]any{}, map[string]string{"key": "cron"})
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "没有可修改的字段" {
		t.Fatalf("空 patch 应 400: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, s.TypesPatch, http.MethodPatch, "/api/admin/notifications/types/cron", map[string]any{"sort": 5, "enabled": 1}, map[string]string{"key": "cron"})
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["ok"] != true {
		t.Fatalf("合法 patch 应 200: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, s.TypesPatch, http.MethodPatch, "/api/admin/notifications/types/nope", map[string]any{"label": "x"}, map[string]string{"key": "nope"})
	if rec.Code != http.StatusNotFound || decodeJSON(t, rec)["error"] != "类别不存在" {
		t.Fatalf("不存在类别应 404: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(t, s.TypesPatch, http.MethodPatch, "/api/admin/notifications/types/cron", map[string]any{"sort": "x"}, map[string]string{"key": "cron"})
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "sort 必须是整数" {
		t.Fatalf("非法 sort 应 400: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentHandlerDownloadAndTraversal(t *testing.T) {
	s := newTestService(t)
	res := testWrite(t, s, writeInput{title: "下载"})
	parsed := parseAttachments([]any{
		map[string]any{"name": "doc.html", "mime": "text/html", "dataBase64": b64([]byte("<h1>报告</h1>"))},
	})
	s.saveNotificationAttachments(context.Background(), res.ID, parsed.items)

	rec := call(t, s.Attachment, http.MethodGet, "/x", nil, map[string]string{"id": strconvFormat(res.ID), "name": "doc.html"})
	if rec.Code != http.StatusOK {
		t.Fatalf("下载应 200: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/html" {
		t.Fatalf("Content-Type 应用入库 mime: %s", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Content-Security-Policy") != "sandbox" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("html 下载应下发安全头: %v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "报告") {
		t.Fatalf("应返回文件内容: %s", rec.Body.String())
	}

	rec = call(t, s.Attachment, http.MethodGet, "/x", nil, map[string]string{"id": strconvFormat(res.ID), "name": "..%2F..%2Fetc%2Fpasswd"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("穿越尝试应 404: %d %s", rec.Code, rec.Body.String())
	}

	rec = call(t, s.Attachment, http.MethodGet, "/x", nil, map[string]string{"id": strconvFormat(res.ID), "name": ""})
	if rec.Code != http.StatusBadRequest || decodeJSON(t, rec)["error"] != "无效的附件名" {
		t.Fatalf("空附件名应 400: %d %s", rec.Code, rec.Body.String())
	}
}

func strconvFormat(id int64) string {
	return strconv.FormatInt(id, 10)
}
