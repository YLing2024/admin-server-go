package apps

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"admin-server-go/internal/config"
)

/* ============ 状态映射 ============ */

func TestHTTPStatusFromCode(t *testing.T) {
	cases := []struct {
		code   int
		expect []int
		want   string
	}{
		{200, nil, StatusUp},
		{301, nil, StatusUp},
		{399, nil, StatusUp},
		{404, []int{200, 404}, StatusUp},
		{500, []int{500}, StatusUp},
		{404, []int{200}, StatusDown},
		{401, nil, StatusAuth},
		{403, nil, StatusAuth},
		{500, nil, StatusDegraded},
		{503, nil, StatusDegraded},
		{400, nil, StatusDown},
		{404, nil, StatusDown},
		{0, nil, StatusDown},
	}
	for _, c := range cases {
		if got := HTTPStatusFromCode(c.code, c.expect); got != c.want {
			t.Errorf("HTTPStatusFromCode(%d,%v)=%q want %q", c.code, c.expect, got, c.want)
		}
	}
}

func TestSystemdAndDockerStatusFromOutput(t *testing.T) {
	sys := []struct {
		in   string
		want string
	}{
		{"active", StatusUp},
		{"activating", StatusDegraded},
		{"reloading", StatusDegraded},
		{"inactive", StatusDown},
		{"failed", StatusDown},
		{"", StatusDown},
	}
	for _, c := range sys {
		if got := SystemdStatusFromOutput(c.in); got != c.want {
			t.Errorf("SystemdStatusFromOutput(%q)=%q want %q", c.in, got, c.want)
		}
	}
	docker := []struct {
		in   string
		want string
	}{
		{"running", StatusUp},
		{"restarting", StatusDegraded},
		{"paused", StatusDegraded},
		{"exited", StatusDown},
		{"dead", StatusDown},
		{"", StatusDown},
	}
	for _, c := range docker {
		if got := DockerStatusFromOutput(c.in); got != c.want {
			t.Errorf("DockerStatusFromOutput(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

/* ============ NormalizeProbe ============ */

func TestNormalizeProbe(t *testing.T) {
	cases := []struct {
		name string
		app  map[string]any
		want *Probe
	}{
		{"http explicit", map[string]any{"probe": map[string]any{"type": "http", "target": "http://127.0.0.1:1/x"}}, &Probe{Type: "http", Target: "http://127.0.0.1:1/x"}},
		{"http expect normalize", map[string]any{"probe": map[string]any{"type": "HTTP", "target": "t", "expect": []any{float64(200), "404"}}}, &Probe{Type: "http", Target: "t", Expect: []int{200, 404}}},
		{"http no target falls back to tcp", map[string]any{"probe": map[string]any{"type": "http"}, "port": float64(8080)}, &Probe{Type: "tcp", Port: 8080}},
		{"systemd via app.unit", map[string]any{"probe": map[string]any{"type": "systemd"}, "unit": "nginx"}, &Probe{Type: "systemd", Unit: "nginx"}},
		{"systemd via probe.unit", map[string]any{"probe": map[string]any{"type": "systemd", "unit": "sshd"}}, &Probe{Type: "systemd", Unit: "sshd"}},
		{"docker", map[string]any{"probe": map[string]any{"type": "docker", "container": "memos"}}, &Probe{Type: "docker", Container: "memos"}},
		{"tcp valid", map[string]any{"probe": map[string]any{"type": "tcp", "port": float64(6379)}}, &Probe{Type: "tcp", Port: 6379}},
		{"tcp zero invalid", map[string]any{"probe": map[string]any{"type": "tcp", "port": float64(0)}}, nil},
		{"tcp negative invalid", map[string]any{"probe": map[string]any{"type": "tcp", "port": float64(-1)}}, nil},
		{"tcp non-number invalid", map[string]any{"probe": map[string]any{"type": "tcp", "port": "abc"}}, nil},
		{"auto container", map[string]any{"container": "c", "unit": "u", "port": float64(1)}, &Probe{Type: "docker", Container: "c"}},
		{"auto unit", map[string]any{"unit": "u", "port": float64(1)}, &Probe{Type: "systemd", Unit: "u"}},
		{"auto port", map[string]any{"port": float64(3000)}, &Probe{Type: "tcp", Port: 3000}},
		{"empty", map[string]any{}, nil},
		{"port zero", map[string]any{"port": float64(0)}, nil},
		{"port string", map[string]any{"port": "x"}, nil},
		{"probe null", map[string]any{"probe": nil}, nil},
		{"probe unknown", map[string]any{"probe": map[string]any{"type": "unknown"}}, nil},
	}
	for _, c := range cases {
		got := NormalizeProbe(c.app)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: NormalizeProbe=%+v want %+v", c.name, got, c.want)
		}
	}
}

/* ============ 公开条目归一化 ============ */

func ptrI64i(v int) *int { return &v }

func TestToPublicApp(t *testing.T) {
	up := ProbeResult{Status: StatusUp, LatencyMs: ptrI64(3)}

	// id 兜底为名称；名称 / 描述截断。
	if got := toPublicApp(map[string]any{"id": "abcd"}, up).Name; got != "abcd" {
		t.Errorf("name fallback=%q", got)
	}
	if got := toPublicApp(map[string]any{"id": "abcd", "name": strings.Repeat("x", 20)}, up).Name; got != strings.Repeat("x", 16) {
		t.Errorf("name truncate=%q", got)
	}
	if got := toPublicApp(map[string]any{}, up).Name; got != "" {
		t.Errorf("name empty=%q", got)
	}
	if got := toPublicApp(map[string]any{"id": "a"}, up).Desc; got != nil {
		t.Errorf("desc default=%v", got)
	}
	if got := toPublicApp(map[string]any{"id": "a", "desc": strings.Repeat("y", 30)}, up).Desc; got == nil || *got != strings.Repeat("y", 24) {
		t.Errorf("desc truncate=%v", got)
	}

	// url / icon：仅非空字符串采用，其余为 null。
	for _, c := range []struct {
		app  map[string]any
		want *string
	}{
		{map[string]any{"id": "a"}, nil},
		{map[string]any{"id": "a", "url": ""}, nil},
		{map[string]any{"id": "a", "url": float64(123)}, nil},
		{map[string]any{"id": "a", "url": "https://x"}, ptrStringT("https://x")},
		{map[string]any{"id": "a", "icon": ""}, nil},
		{map[string]any{"id": "a", "icon": float64(5)}, nil},
		{map[string]any{"id": "a", "icon": "管"}, ptrStringT("管")},
	} {
		v := toPublicApp(c.app, up)
		var got *string
		if _, has := c.app["url"]; has {
			got = v.URL
		} else {
			got = v.Icon
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("app=%v got=%v want=%v", c.app, got, c.want)
		}
	}

	// onDemand：严格 === true。
	if !toPublicApp(map[string]any{"id": "a", "onDemand": true}, up).OnDemand {
		t.Error("onDemand true")
	}
	for _, bad := range []any{"true", float64(1), nil} {
		if toPublicApp(map[string]any{"id": "a", "onDemand": bad}, up).OnDemand {
			t.Errorf("onDemand %v should be false", bad)
		}
	}

	// category / status / latency 透传。
	v := toPublicApp(map[string]any{"id": "a", "category": "self"}, ProbeResult{Status: StatusDown})
	if v.Category != "self" || v.Status != "down" || v.LatencyMs != nil {
		t.Errorf("normalization=%+v", v)
	}
}

func ptrStringT(s string) *string { return &s }

/* ============ 分类聚合 ============ */

func TestAggregateCategories(t *testing.T) {
	declared := []any{
		map[string]any{"id": "self", "name": "自研"},
		map[string]any{"id": "infra"},
	}
	apps := []PublicAppView{
		{ID: "a", Category: "self"},
		{ID: "b", Category: "__missing"},
		{ID: "c", Category: "infra"},
	}
	cats, got := aggregateCategories(declared, apps)
	wantCats := []PublicCategory{{ID: "self", Name: "自研"}, {ID: "infra", Name: "infra"}, {ID: otherCategoryID, Name: otherCategoryName}}
	if !reflect.DeepEqual(cats, wantCats) {
		t.Fatalf("categories=%+v want %+v", cats, wantCats)
	}
	if got[1].Category != otherCategoryID {
		t.Errorf("unknown category should be other: %+v", got[1])
	}
	// 已知分类不注入 other。
	cats2, _ := aggregateCategories(declared, []PublicAppView{{ID: "a", Category: "self"}})
	if len(cats2) != 2 {
		t.Errorf("declared-only categories=%+v", cats2)
	}
}

/* ============ 公开响应 ============ */

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestPublicAppView_Whitelist(t *testing.T) {
	pub := PublicAppView{
		ID: "a", Name: "A", Category: "self", Desc: ptrStringT("d"), URL: ptrStringT("https://example.com"),
		Icon: ptrStringT("home"), OnDemand: true, Status: "up", LatencyMs: ptrI64(3),
	}
	wantKeys := []string{"category", "desc", "icon", "id", "latencyMs", "name", "onDemand", "status", "url"}
	if got := jsonKeys(t, pub); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("keys=%v want %v", got, wantKeys)
	}
	if got := jsonKeys(t, PublicPayload{}); !reflect.DeepEqual(got, []string{"apps", "categories"}) {
		t.Fatalf("payload keys=%v", got)
	}
}

func TestRefreshLimiter(t *testing.T) {
	l := NewRefreshLimiter(10000)
	const t0 int64 = 1_000_000
	if !l.Allow("1.2.3.4", t0) {
		t.Error("first allow")
	}
	if l.Allow("1.2.3.4", t0+1) {
		t.Error("within window should be denied")
	}
	if l.Allow("1.2.3.4", t0+9999) {
		t.Error("just before window should be denied")
	}
	if !l.Allow("1.2.3.4", t0+10000) {
		t.Error("window elapsed should allow")
	}
	if !l.Allow("5.6.7.8", t0+10001) {
		t.Error("different key should allow")
	}
}

/* ============ 登记表读取 ============ */

func testService(appsFile string) *Service {
	return &Service{
		cfg: &config.Config{AppsFile: appsFile, AuditLog: filepath.Join(filepath.Dir(appsFile), "audit.log")},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestReadRegistry(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	s := testService(missing)
	exists, data, err := s.readRegistry()
	if err != nil || exists || data != nil {
		t.Errorf("missing: exists=%v data=%v err=%v", exists, data, err)
	}

	p := filepath.Join(dir, "apps.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"apps":[{"id":"a"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s = testService(p)
	exists, data, err = s.readRegistry()
	if err != nil || !exists || data == nil {
		t.Fatalf("valid: exists=%v data=%v err=%v", exists, data, err)
	}
	apps, _ := data["apps"].([]any)
	if len(apps) != 1 {
		t.Errorf("data=%v", data)
	}

	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = testService(broken)
	if _, _, err := s.readRegistry(); err == nil || !strings.HasPrefix(err.Error(), "应用登记表解析失败: ") {
		t.Errorf("broken err=%v", err)
	}
}

/* ============ HTTP 处理函数 ============ */

func handlerService(t *testing.T, appsFile string) *Service {
	t.Helper()
	cfg := &config.Config{AppsFile: appsFile, AuditLog: filepath.Join(t.TempDir(), "audit.log")}
	s := New(Options{Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s.tcpProbe = func(int) bool { return false }
	return s
}

func TestPublicApps_Handlers(t *testing.T) {
	// 缺失登记表 → 200 + 空载荷 + Cache-Control: no-store。
	s := handlerService(t, filepath.Join(t.TempDir(), "missing.json"))
	rec := httptest.NewRecorder()
	s.PublicApps(rec, httptest.NewRequest(http.MethodGet, "/api/public/apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("missing status=%d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control=%q", cc)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"apps", "categories"}) {
		t.Errorf("keys=%v", keys)
	}
	if apps, ok := body["apps"].([]any); !ok || len(apps) != 0 {
		t.Errorf("apps=%v", body["apps"])
	}

	// 解析失败 → 503 + 通用文案。
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	_ = os.WriteFile(broken, []byte("{ not json"), 0o644)
	s = handlerService(t, broken)
	rec = httptest.NewRecorder()
	s.PublicApps(rec, httptest.NewRequest(http.MethodGet, "/api/public/apps", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken status=%d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "应用数据暂不可用" {
		t.Errorf("error=%v", body["error"])
	}

	// 正常登记表：tcp 探活失败 → down；onDemand → idle；hidden 不返回。
	reg := filepath.Join(dir, "apps.json")
	_ = os.WriteFile(reg, []byte(`{"categories":[{"id":"self","name":"自研"}],"apps":[{"id":"a","category":"self","probe":{"type":"tcp","port":1}},{"id":"b","onDemand":true,"probe":{"type":"tcp","port":1}},{"id":"c","hidden":true}]}`), 0o644)
	s = handlerService(t, reg)
	rec = httptest.NewRecorder()
	s.PublicApps(rec, httptest.NewRequest(http.MethodGet, "/api/public/apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid status=%d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	apps := body["apps"].([]any)
	if len(apps) != 2 {
		t.Fatalf("apps=%v", apps)
	}
	byID := map[string]map[string]any{}
	for _, a := range apps {
		am := a.(map[string]any)
		byID[am["id"].(string)] = am
	}
	if byID["a"]["status"] != StatusDown {
		t.Errorf("a status=%v", byID["a"]["status"])
	}
	if byID["b"]["status"] != StatusIdle {
		t.Errorf("b status=%v", byID["b"]["status"])
	}
	if _, leaked := byID["a"]["port"]; leaked {
		t.Error("port must not leak")
	}
	if _, leaked := body["discovered"]; leaked {
		t.Error("discovered must not leak")
	}
	if _, leaked := body["registryPath"]; leaked {
		t.Error("registryPath must not leak")
	}
}
