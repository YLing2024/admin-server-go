package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// newTestRoute 用与 routeTable 相同的方式解析 pattern，构造只含路由表、不含业务依赖的 Server。
func newTestRoute(patterns ...string) *Server {
	routes := make([]apiRoute, 0, len(patterns))
	for _, p := range patterns {
		rt := apiRoute{pattern: p}
		rt.method, rt.routePath = splitPattern(p)
		routes = append(routes, rt)
	}
	return &Server{routes: routes}
}

// TestAllowedMethods 复刻 Express 对未匹配 OPTIONS 的 Allow 计算：按注册顺序收集路径命中的方法、
// 去重、GET 后追加 HEAD，`ALL` 不参与。
func TestAllowedMethods(t *testing.T) {
	s := newTestRoute(
		"GET /api/admin/api-tokens",
		"POST /api/admin/api-tokens",
		"PATCH /api/admin/api-tokens/:id",
		"DELETE /api/admin/api-tokens/:id",
		"ALL /api/admin/sessions*",
		"GET /api/public/apps",
	)

	cases := []struct {
		path string
		want []string
	}{
		{"/api/admin/api-tokens", []string{"GET", "HEAD", "POST"}},
		{"/api/admin/api-tokens/42", []string{"PATCH", "DELETE"}},
		{"/api/admin/sessions/abc", nil}, // ALL 不贡献
		{"/definitely-nope", nil},
		{"/api/public/apps/", []string{"GET", "HEAD"}}, // 尾部斜杠等价
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodOptions, c.path, nil)
		got := s.allowedMethods(req)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("allowedMethods(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestHeadWriterContentLength 校验 HEAD 承接器：丢弃正文、Content-Length 等于 GET 会产生的正文长度。
func TestHeadWriterContentLength(t *testing.T) {
	rec := httptest.NewRecorder()
	hw := &headWriter{ResponseWriter: rec}
	hw.Header().Set("Content-Type", "application/json; charset=utf-8")
	body := `{"ok":true}`
	if _, err := hw.Write([]byte(body)); err != nil {
		t.Fatalf("write: %v", err)
	}
	hw.finish()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 正文应为空，得到 %q", rec.Body.String())
	}
}

// TestHeadWriterNoContent 校验 204/304 不补 Content-Length（无正文响应）。
func TestHeadWriterNoContent(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		rec := httptest.NewRecorder()
		hw := &headWriter{ResponseWriter: rec}
		hw.WriteHeader(code)
		hw.finish()
		if rec.Code != code {
			t.Fatalf("status = %d, want %d", rec.Code, code)
		}
		if cl := rec.Header().Get("Content-Length"); cl != "" {
			t.Fatalf("code %d Content-Length = %q, want 空", code, cl)
		}
	}
}

// TestMatchPath 校验路径匹配：单段占位、尾部通配、尾部斜杠归一。
func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		wantOK  bool
		param   string
	}{
		{"GET /api/admin/api-tokens/:id", "/api/admin/api-tokens/42", true, "42"},
		{"GET /api/admin/api-tokens/:id", "/api/admin/api-tokens", false, ""},
		{"GET /api/admin/api-tokens/:id", "/api/admin/api-tokens/42/x", false, ""},
		{"ALL /api/admin/sessions*", "/api/admin/sessions", true, ""},
		{"ALL /api/admin/sessions*", "/api/admin/sessions/abc/pty", true, ""},
		{"ALL /api/admin/sessions*", "/api/admin/sessionsX", false, ""},
		{"GET /api/admin/api-tokens", "/api/admin/api-tokens/", true, ""},
	}
	for _, c := range cases {
		rt := apiRoute{pattern: c.pattern}
		rt.method, rt.routePath = splitPattern(c.pattern)
		params, ok := matchPath(&rt, c.path)
		if ok != c.wantOK {
			t.Errorf("matchPath(%q, %q) ok=%v, want %v", c.pattern, c.path, ok, c.wantOK)
			continue
		}
		if ok && c.param != "" && params["id"] != c.param {
			t.Errorf("matchPath(%q, %q)[id] = %q, want %q", c.pattern, c.path, params["id"], c.param)
		}
	}
}
