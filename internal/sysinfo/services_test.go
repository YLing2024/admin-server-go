package sysinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseRunningUnits(t *testing.T) {
	text := `[{"unit":"admin-server-go.service","active":"active","sub":"running"},
{"unit":" ssh.service ","active":"active"},{"unit":""}]`
	units := parseRunningUnits(text)
	if len(units) != 2 {
		t.Fatalf("units = %d, want 2: %+v", len(units), units)
	}
	if units[1].Unit != "ssh.service" || units[0].Active != "active" {
		t.Errorf("unexpected units: %+v", units)
	}
	if got := parseRunningUnits("not json"); len(got) != 0 {
		t.Errorf("invalid json should return empty, got %+v", got)
	}
}

func TestParseCPUUsageUsec(t *testing.T) {
	text := "usage_usec 301411237\nuser_usec 99404043\nsystem_usec 202007193\n"
	got, ok := parseCPUUsageUsec(text)
	if !ok || got != 301411237 {
		t.Fatalf("usage = %d/%v, want 301411237/true", got, ok)
	}
	if _, ok := parseCPUUsageUsec("user_usec 1\n"); ok {
		t.Error("missing usage_usec should be !ok")
	}
	if _, ok := parseCPUUsageUsec("usage_usec abc\n"); ok {
		t.Error("invalid usage_usec should be !ok")
	}
}

func TestCPUPercent(t *testing.T) {
	// 1 秒窗口、8 核：累计 8e6 usec 恰好占满整机 100%。
	if got := cpuPercent(0, 8_000_000, time.Second, 8); got != 100 {
		t.Errorf("full machine = %v, want 100", got)
	}
	// 单核满载一个窗口 = 100/8 = 12.5%。
	if got := cpuPercent(100_000, 1_100_000, time.Second, 8); got != 12.5 {
		t.Errorf("one core = %v, want 12.5", got)
	}
	if got := cpuPercent(200, 100, time.Second, 8); got != 0 {
		t.Errorf("counter reset should be 0, got %v", got)
	}
	if got := cpuPercent(0, 1_000_000, 0, 8); got != 0 {
		t.Errorf("zero elapsed should be 0, got %v", got)
	}
}

func TestParseMemoryMB(t *testing.T) {
	if mb := parseMemoryMB("33382400\n"); mb == nil || *mb != 32 {
		t.Errorf("memory = %v, want 32", mb)
	}
	for _, bad := range []string{"", "  ", "NaN", "-1", "inf"} {
		if mb := parseMemoryMB(bad); mb != nil {
			t.Errorf("parseMemoryMB(%q) = %v, want nil", bad, *mb)
		}
	}
}

func TestServicesHandlerDegrades(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	s := New(nil)
	start := time.Now()
	rec := httptest.NewRecorder()
	s.Services(rec, httptest.NewRequest(http.MethodGet, "/api/admin/system/services", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > time.Second {
		t.Errorf("degrade took %v, want <1s", elapsed)
	}
	var rows []RunningService
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want empty", rows)
	}
}
