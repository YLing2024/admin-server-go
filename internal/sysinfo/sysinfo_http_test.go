package sysinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// TestInfoHandler 走真实 HTTP 处理链路，确认响应 200 且核心字段可解析。
func TestInfoHandler(t *testing.T) {
	s := New(nil)
	rec := httptest.NewRecorder()
	s.Info(rec, httptest.NewRequest(http.MethodGet, "/api/admin/system/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var info Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.OS == "" || info.Kernel == "" || info.Arch == "" || info.Hostname == "" {
		t.Errorf("基础字段为空: %+v", info)
	}
	if info.Memory.Total <= 0 || info.CPUCores != runtime.NumCPU() || len(info.Load) != 3 {
		t.Errorf("资源字段异常: mem=%+v cores=%d load=%v", info.Memory, info.CPUCores, info.Load)
	}
	if info.Disks == nil || info.Ports.Items == nil || info.Systemd.Failed == nil || info.Logins.Items == nil {
		t.Errorf("数组字段不得为 null: disks=%v ports=%v systemd=%v logins=%v",
			info.Disks, info.Ports.Items, info.Systemd.Failed, info.Logins.Items)
	}
}

// TestInfoHandlerDegrades 模拟外部命令全部不可用：接口仍 200，对应字段为空并带 error。
func TestInfoHandlerDegrades(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	s := New(nil)
	rec := httptest.NewRecorder()
	s.Info(rec, httptest.NewRequest(http.MethodGet, "/api/admin/system/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var info Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Ports.Error == "" || len(info.Ports.Items) != 0 || info.Ports.Total != 0 {
		t.Errorf("ports 未降级: %+v", info.Ports)
	}
	if info.Systemd.Error == "" || len(info.Systemd.Failed) != 0 {
		t.Errorf("systemd 未降级: %+v", info.Systemd)
	}
	if info.Logins.Error == "" || len(info.Logins.Items) != 0 {
		t.Errorf("logins 未降级: %+v", info.Logins)
	}
}

// TestProcessesHandler 确认 Top 进程接口在 2s 内完成，且 CPU 占比总和不超过整机核数上限。
func TestProcessesHandler(t *testing.T) {
	s := New(nil)
	start := time.Now()
	rec := httptest.NewRecorder()
	s.Processes(rec, httptest.NewRequest(http.MethodGet, "/api/admin/system/processes", nil))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %v 超过 2s", elapsed)
	}
	var payload ProcessesPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error != "" {
		t.Fatalf("采集失败: %s", payload.Error)
	}
	if len(payload.Processes) == 0 {
		t.Fatal("Top 进程为空")
	}
	sum := 0.0
	for _, p := range payload.Processes {
		sum += p.CPUPercent
	}
	if sum > float64(runtime.NumCPU())*100 {
		t.Errorf("CPU 占比之和 %.1f 超过 %d×100", sum, runtime.NumCPU())
	}
}
