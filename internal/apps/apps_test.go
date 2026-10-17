package apps

import (
	"bytes"
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

/* ============ httpStatusFromCode ============ */

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

/* ============ ScanDiscovered ============ */

const ssSample = "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
	"LISTEN 0      4096       127.0.0.1:3100       0.0.0.0:*    users:((\"node\",pid=1234,fd=20))\n" +
	"LISTEN 0      128        127.0.0.1:6379       0.0.0.0:*    users:((\"redis-server\",pid=999,fd=6))\n" +
	"LISTEN 0      128          0.0.0.0:22         0.0.0.0:*    users:((\"sshd\",pid=1,fd=3))\n" +
	"LISTEN 0      128            [::1]:8080         [::]:*    users:((\"foo\",pid=2,fd=4))"

func TestScanDiscovered(t *testing.T) {
	got := ScanDiscovered(ssSample, nil, nil, nil)
	want := []DiscoveredEntry{
		{Port: 3100, Process: ptrString("node")},
		{Port: 6379, Process: ptrString("redis-server")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("basic=%+v want %+v", got, want)
	}
	if got := ScanDiscovered(ssSample, []any{float64(3100)}, nil, nil); !reflect.DeepEqual(got, []DiscoveredEntry{{Port: 6379, Process: ptrString("redis-server")}}) {
		t.Errorf("ignorePorts=%+v", got)
	}
	if got := ScanDiscovered(ssSample, nil, nil, []any{float64(6379)}); !reflect.DeepEqual(got, []DiscoveredEntry{{Port: 3100, Process: ptrString("node")}}) {
		t.Errorf("occupied=%+v", got)
	}
}

func TestScanDiscovered_Exclusions(t *testing.T) {
	lines := strings.Join([]string{
		"LISTEN 0 128 127.0.0.1:5000 0.0.0.0:* users:((\"docker-proxy\",pid=7,fd=1))",
		"LISTEN 0 128 127.0.0.1:6000 0.0.0.0:* users:((\"chrome\",pid=8,fd=1))",
		"LISTEN 0 128 127.0.0.1:7000 0.0.0.0:* users:((\"nginx\",pid=9,fd=1))",
	}, "\n")
	want := []DiscoveredEntry{{Port: 7000, Process: ptrString("nginx")}}
	if got := ScanDiscovered(lines, nil, []any{"^chrome"}, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("ignoreProcesses=%+v want %+v", got, want)
	}
	// 非法正则应被忽略且不抛错。
	if got := ScanDiscovered(lines[:0]+lines, nil, []any{"("}, nil); len(got) == 0 {
		t.Errorf("invalid regex should not drop all: %+v", got)
	}
	// 无 users 字段 → process null，仍计入。
	if got := ScanDiscovered("LISTEN 0 128 127.0.0.1:9000 0.0.0.0:*", nil, nil, nil); !reflect.DeepEqual(got, []DiscoveredEntry{{Port: 9000}}) {
		t.Errorf("no users=%+v", got)
	}
}

func TestScanDiscovered_DedupSortLimit(t *testing.T) {
	dup := "LISTEN 0 128 127.0.0.1:2222 0.0.0.0:* users:((\"a\",pid=1,fd=1))\n" +
		"LISTEN 0 128 127.0.0.1:2222 0.0.0.0:* users:((\"b\",pid=2,fd=2))"
	if got := ScanDiscovered(dup, nil, nil, nil); !reflect.DeepEqual(got, []DiscoveredEntry{{Port: 2222, Process: ptrString("a")}}) {
		t.Errorf("dedup=%+v", got)
	}
	var many []string
	for n := 0; n < 40; n++ {
		many = append(many, "LISTEN 0 128 127.0.0.1:"+itoa(1000+n)+" 0.0.0.0:* users:((\"p"+itoa(n)+"\",pid="+itoa(n)+",fd=1))")
	}
	out := ScanDiscovered(strings.Join(many, "\n"), nil, nil, nil)
	if len(out) != 30 || out[0].Port != 1000 || out[29].Port != 1029 {
		t.Errorf("limit len=%d first=%d last=%d", len(out), out[0].Port, out[29].Port)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

/* ============ ToAppView ============ */

func TestToAppView(t *testing.T) {
	ok := ProbeResult{Status: "up", ProbeType: ptrString("tcp"), LatencyMs: ptrI64(3), Detail: ptrString("TCP 3100 3ms")}

	if got := ToAppView(map[string]any{"id": "abcd"}, ok).Name; got != "abcd" {
		t.Errorf("name fallback=%q", got)
	}
	long := strings.Repeat("x", 20)
	if got := ToAppView(map[string]any{"id": "abcd", "name": long}, ok).Name; got != strings.Repeat("x", 16) {
		t.Errorf("name truncate=%q", got)
	}
	if got := ToAppView(map[string]any{}, ok).Name; got != "" {
		t.Errorf("name empty=%q", got)
	}
	if got := ToAppView(map[string]any{"id": "a"}, ok).Desc; got != nil {
		t.Errorf("desc default=%v", got)
	}
	if got := ToAppView(map[string]any{"id": "a", "desc": strings.Repeat("y", 30)}, ok).Desc; got == nil || *got != strings.Repeat("y", 24) {
		t.Errorf("desc truncate=%v", got)
	}
	for _, c := range []struct {
		app  map[string]any
		want *string
	}{
		{map[string]any{"id": "a"}, nil},
		{map[string]any{"id": "a", "url": ""}, nil},
		{map[string]any{"id": "a", "url": float64(123)}, nil},
		{map[string]any{"id": "a", "url": "https://x"}, ptrString("https://x")},
		{map[string]any{"id": "a", "icon": ""}, nil},
		{map[string]any{"id": "a", "icon": float64(5)}, nil},
		{map[string]any{"id": "a", "icon": "管"}, ptrString("管")},
	} {
		v := ToAppView(c.app, ok)
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
	if ToAppView(map[string]any{"id": "a", "onDemand": true}, ok).OnDemand != true {
		t.Error("onDemand true")
	}
	for _, bad := range []any{"true", float64(1), nil} {
		if ToAppView(map[string]any{"id": "a", "onDemand": bad}, ok).OnDemand {
			t.Errorf("onDemand %v should be false", bad)
		}
	}
}

func TestToAppView_PortTagsCategoryError(t *testing.T) {
	ok := ProbeResult{Status: "up", ProbeType: ptrString("tcp"), LatencyMs: ptrI64(3), Detail: ptrString("TCP 3100 3ms")}
	portCases := []struct {
		in   any
		want *int
	}{
		{float64(3100), ptrI64i(3100)},
		{"3100", ptrI64i(3100)},
		{float64(0), nil},
		{float64(-1), nil},
		{"x", nil},
		{nil, nil},
	}
	for _, c := range portCases {
		app := map[string]any{"id": "a"}
		if c.in != nil {
			app["port"] = c.in
		}
		if got := ToAppView(app, ok).Port; !reflect.DeepEqual(got, c.want) {
			t.Errorf("port in=%v got=%v want=%v", c.in, got, c.want)
		}
	}
	if got := ToAppView(map[string]any{"id": "a", "tags": []any{"x", float64(1), nil}}, ok).Tags; !reflect.DeepEqual(got, []string{"x", "1", "null"}) {
		t.Errorf("tags=%v", got)
	}
	if got := ToAppView(map[string]any{"id": "a", "tags": "nope"}, ok).Tags; !reflect.DeepEqual(got, []string{}) {
		t.Errorf("tags non-array=%v", got)
	}
	v := ToAppView(map[string]any{"id": "a"}, ProbeResult{Status: "down", Detail: ptrString("d"), Error: ptrString("")})
	if v.Category != "" || v.Status != "down" || v.ProbeType != nil || v.LatencyMs != nil || v.Detail == nil || *v.Detail != "d" || v.Error != nil {
		t.Errorf("normalization=%+v", v)
	}
}

func ptrI64i(v int) *int { return &v }

/* ============ 公开面板 ============ */

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

func TestToPublicApp_Whitelist(t *testing.T) {
	pub := ToPublicApp(AppView{
		ID: "a", Name: "A", Category: "self", Desc: ptrString("d"), URL: ptrString("https://example.com"),
		Icon: ptrString("home"), OnDemand: true, Port: ptrI64i(3100), Tags: []string{"x"},
		Status: "up", ProbeType: ptrString("tcp"), LatencyMs: ptrI64(3), Detail: ptrString("TCP 3100 3ms"),
	})
	wantKeys := []string{"category", "desc", "icon", "id", "latencyMs", "name", "onDemand", "status", "url"}
	if got := jsonKeys(t, pub); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("keys=%v want %v", got, wantKeys)
	}
	b, _ := json.Marshal(pub)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["status"] != "up" || m["onDemand"] != true || m["latencyMs"] != float64(3) {
		t.Errorf("payload=%v", m)
	}
	if _, has := m["port"]; has {
		t.Error("port must not leak")
	}
}

func TestToPublicApp_IdlePassthrough(t *testing.T) {
	pub := ToPublicApp(AppView{ID: "a", Name: "A", Category: "self", Status: StatusIdle, OnDemand: true})
	if pub.Status != StatusIdle || !pub.OnDemand {
		t.Errorf("pub=%+v", pub)
	}
}

func TestToPublicPayload(t *testing.T) {
	payload := &collectPayload{
		GeneratedAt:   "x",
		RegistryPath:  "/internal/data/apps.json",
		RegistryMtime: floatPtr(123),
		Notice:        ptrString("未找到应用登记表"),
		Warning:       ptrString("重复 id: a"),
		Categories:    []CategoryAgg{{ID: "self", Name: "自研", Total: 2, Up: 1, Down: 1}},
		Apps: []AppView{{
			ID: "a", Name: "A", Category: "self", Status: "down",
			Port: ptrI64i(3100), Tags: []string{}, ProbeType: ptrString("tcp"), Detail: ptrString("x"), Error: ptrString("y"),
		}},
		Discovered: []DiscoveredEntry{{Port: 9999, Process: ptrString("node")}},
	}
	pub := ToPublicPayload(payload)
	if got := jsonKeys(t, pub); !reflect.DeepEqual(got, []string{"apps", "categories"}) {
		t.Fatalf("top keys=%v", got)
	}
	b, _ := json.Marshal(pub)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	cats := m["categories"].([]any)
	if len(cats) != 1 || !reflect.DeepEqual(cats[0], map[string]any{"id": "self", "name": "自研"}) {
		t.Errorf("categories=%v", cats)
	}
	if _, has := m["discovered"]; has {
		t.Error("discovered must not leak")
	}
	if _, has := m["registryPath"]; has {
		t.Error("registryPath must not leak")
	}
	if _, has := m["notice"]; has {
		t.Error("notice must not leak")
	}
	if _, has := m["warning"]; has {
		t.Error("warning must not leak")
	}
	app := m["apps"].([]any)[0].(map[string]any)
	if _, has := app["port"]; has {
		t.Error("port must not leak")
	}
}

func floatPtr(v float64) *float64 { return &v }

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

func TestRegistryPathAndReadRegistry(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	s := testService(missing)
	if s.registryPath() != missing {
		t.Errorf("registryPath=%q want %q", s.registryPath(), missing)
	}
	exists, data, mtime, err := s.readRegistry()
	if err != nil || exists || data != nil || mtime != nil {
		t.Errorf("missing: exists=%v data=%v mtime=%v err=%v", exists, data, mtime, err)
	}

	p := filepath.Join(dir, "apps.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"apps":[{"id":"a"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s = testService(p)
	exists, data, mtime, err = s.readRegistry()
	if err != nil || !exists || data == nil || mtime == nil {
		t.Fatalf("valid: exists=%v data=%v mtime=%v err=%v", exists, data, mtime, err)
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
	if _, _, _, err := s.readRegistry(); err == nil || !strings.HasPrefix(err.Error(), "应用登记表解析失败: ") {
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

func TestAdminApps_Handlers(t *testing.T) {
	// 缺失登记表 → 200 + apps:[] + notice。
	s := handlerService(t, filepath.Join(t.TempDir(), "missing.json"))
	rec := httptest.NewRecorder()
	s.AdminApps(rec, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("missing status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if apps, ok := body["apps"].([]any); !ok || len(apps) != 0 {
		t.Errorf("apps=%v", body["apps"])
	}
	if body["notice"] == nil {
		t.Errorf("notice should be set: %v", body["notice"])
	}

	// 解析失败 → 500 + {error}。
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = handlerService(t, broken)
	rec = httptest.NewRecorder()
	s.AdminApps(rec, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("broken status=%d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	msg, _ := body["error"].(string)
	if !strings.HasPrefix(msg, "应用登记表解析失败: ") {
		t.Errorf("error=%q", msg)
	}

	// 正常登记表：tcp 探活失败 → down；onDemand → idle。
	reg := filepath.Join(dir, "apps.json")
	_ = os.WriteFile(reg, []byte(`{"apps":[{"id":"a","probe":{"type":"tcp","port":1}},{"id":"b","onDemand":true,"probe":{"type":"tcp","port":1}}]}`), 0o644)
	s = handlerService(t, reg)
	rec = httptest.NewRecorder()
	s.AdminApps(rec, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
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
}

func TestPublicApps_Handlers(t *testing.T) {
	// 缺失登记表 → 200，字段白名单，Cache-Control: no-store。
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
}

/* ============ 顶层键顺序 ============ */

// topLevelKeyOrder 从原始 JSON 中按出现顺序取出顶层键（map 会丢顺序，必须走流式解码）。
func topLevelKeyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("期望对象，得到 %v", tok)
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, keyTok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

// TestCollectPayloadKeyOrder 断言 apps 载荷顶层键顺序与旧实现一致：旧实现在 collectOnce
// 结果上展开 `{ ...payload, cached }`，故 cached 永远排在最后（不是第二个）。
func TestCollectPayloadKeyOrder(t *testing.T) {
	p := collectPayload{
		GeneratedAt:  "2026-01-01T00:00:00.000Z",
		TTLSeconds:   10,
		RegistryPath: "/internal/data/apps.json",
		Categories:   []CategoryAgg{},
		Apps:         []AppView{},
		Discovered:   []DiscoveredEntry{},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"generatedAt", "ttlSeconds", "registryPath", "registryMtime", "notice", "warning", "categories", "apps", "discovered", "cached"}
	if got := topLevelKeyOrder(t, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("顶层键顺序 = %v, want %v", got, want)
	}
}
