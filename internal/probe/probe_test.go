package probe

import (
	"net"
	"testing"

	"admin-server-go/internal/config"
)

func TestExtractBracketed(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"[lzo] lz4", "lzo"},
		{"lz4 [zstd]", "zstd"},
		{"zstd", "zstd"},
		{"", nil},
		{"   ", nil},
	}
	for _, c := range cases {
		if got := extractBracketed(c.in); got != c.want {
			t.Errorf("extractBracketed(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestIsWholeDisk(t *testing.T) {
	cases := map[string]bool{
		"sda": true, "vdb": true, "hdc": true,
		"sda1": false, "nvme0n1": false, "xda": false, "sd1": false, "": false,
	}
	for in, want := range cases {
		if got := isWholeDisk(in); got != want {
			t.Errorf("isWholeDisk(%q)=%v want %v", in, got, want)
		}
	}
}

func TestCheckTCPPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s := New(&config.Config{}, nil)
	if !s.CheckTCPPort(port, 500) {
		t.Errorf("open port %d should be reachable", port)
	}
	_ = ln.Close()
	if s.CheckTCPPort(port, 200) {
		t.Errorf("closed port %d should not be reachable", port)
	}
}

func TestCollectServices(t *testing.T) {
	s := New(&config.Config{Port: 19265}, nil)
	got, err := s.CollectServices()
	if err != nil {
		t.Fatalf("CollectServices: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("services=%d want 8", len(got))
	}
	if got[0].Name != "admin-server" || got[0].Status != "up" {
		t.Errorf("self entry=%+v", got[0])
	}
	for _, svc := range got {
		switch svc.Status {
		case "up", "down":
		default:
			t.Errorf("unexpected status %q for %s", svc.Status, svc.Name)
		}
	}
}

func TestReadMeminfoAndPercents(t *testing.T) {
	m := ReadMeminfo()
	if m.Total <= 0 {
		t.Fatalf("total=%v want > 0", m.Total)
	}
	if m.Used < 0 {
		t.Errorf("used=%v", m.Used)
	}
	if p := MemPercent(); p < 0 || p > 100 {
		t.Errorf("MemPercent out of range: %v", p)
	}
	if p := SwapPercent(); p < 0 || p > 100 {
		t.Errorf("SwapPercent out of range: %v", p)
	}
}
