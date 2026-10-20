package sysinfo

import (
	"context"
	"testing"
)

func TestParseSSOutput(t *testing.T) {
	text := `State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
LISTEN 0      4096         0.0.0.0:22        0.0.0.0:*     users:(("sshd",pid=812,fd=3))
LISTEN 0      511            [::]:443          [::]:*      users:(("nginx",pid=900,fd=6))
LISTEN 0      4096            *:8080            *:*        users:(("java",pid=1001,fd=10))
ESTAB  0      0        127.0.0.1:1234    127.0.0.1:22
`
	items := parseSSOutput(text)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	if items[0].Port != 22 || items[0].Process != "sshd" || items[0].Address != "0.0.0.0" {
		t.Errorf("first item = %+v", items[0])
	}
	if items[0].PID == nil || *items[0].PID != 812 {
		t.Errorf("first pid = %v", items[0].PID)
	}
	if items[1].Port != 443 || items[1].Address != "[::]" {
		t.Errorf("second item = %+v", items[1])
	}
	if items[2].Port != 8080 || items[2].Process != "java" {
		t.Errorf("third item = %+v", items[2])
	}
}

func TestSplitLocalAddress(t *testing.T) {
	cases := []struct {
		in      string
		address string
		port    int
	}{
		{"0.0.0.0:22", "0.0.0.0", 22},
		{"[::]:443", "[::]", 443},
		{"*:80", "*", 80},
		{"bad", "bad", 0},
		{"1.2.3.4:", "1.2.3.4:", 0},
		{"1.2.3.4:abc", "1.2.3.4", 0},
	}
	for _, c := range cases {
		addr, port := splitLocalAddress(c.in)
		if addr != c.address || port != c.port {
			t.Errorf("splitLocalAddress(%q) = (%q, %d), want (%q, %d)", c.in, addr, port, c.address, c.port)
		}
	}
}

func TestParseFailedUnits(t *testing.T) {
	text := `foo.service loaded failed failed Foo daemon
bar.service loaded failed failed Bar daemon with spaces
`
	units := parseFailedUnits(text)
	if len(units) != 2 {
		t.Fatalf("units = %d, want 2", len(units))
	}
	if units[0].Name != "bar.service" || units[1].Name != "foo.service" {
		t.Errorf("units not sorted by name: %+v", units)
	}
	if units[1].Description != "Foo daemon" {
		t.Errorf("description = %q", units[1].Description)
	}
}

func TestParseLastOutput(t *testing.T) {
	text := `root     pts/0        203.0.113.5      Tue Oct 20 09:00   still logged in
alice    pts/1        -                Mon Oct 19 08:00 - 09:00 (01:00)
bob      pts/2        :0               Sun Oct 18 07:00   still logged in
reboot   system boot  6.12.107         Sat Oct 17 06:00   still running

wtmp begins Sat Oct 17 06:00:00 2026
`
	items := parseLastOutput(text)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3: %+v", len(items), items)
	}
	if items[0].User != "root" || items[0].IP != "203.0.113.5" {
		t.Errorf("first = %+v", items[0])
	}
	if items[1].User != "alice" || items[1].IP != "" {
		t.Errorf("second = %+v", items[1])
	}
	if items[2].User != "bob" || items[2].IP != "" {
		t.Errorf("third = %+v", items[2])
	}
	if items[0].Time == "" {
		t.Errorf("time should not be empty")
	}
}

func TestParseProcStat(t *testing.T) {
	line := "123 (worker (x)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15"
	comm, ticks, ok := parseProcStat(line)
	if !ok {
		t.Fatal("parseProcStat failed")
	}
	if comm != "worker (x)" {
		t.Errorf("comm = %q", comm)
	}
	// rest[11] = 12, rest[12] = 13
	if ticks != 23 {
		t.Errorf("ticks = %v, want 23", ticks)
	}
}

func TestParseProcStatus(t *testing.T) {
	text := "Name:\tnginx\nVmRSS:\t  4567 kB\n"
	name, rss := parseProcStatus(text)
	if name != "nginx" || rss != 4567 {
		t.Errorf("name=%q rss=%d", name, rss)
	}
}

func TestMergeTopDedupes(t *testing.T) {
	rows := []TopProcess{
		{PID: 1, Name: "a", CPUPercent: 50, MemMB: 10},
		{PID: 2, Name: "b", CPUPercent: 20, MemMB: 100},
		{PID: 3, Name: "c", CPUPercent: 10, MemMB: 50},
	}
	got := mergeTop(rows, 2)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	seen := map[int]bool{}
	for _, p := range got {
		if seen[p.PID] {
			t.Errorf("duplicate pid %d", p.PID)
		}
		seen[p.PID] = true
	}
	if got[0].PID != 1 || got[1].PID != 2 || got[2].PID != 3 {
		t.Errorf("order = %+v", got)
	}
}

func TestDisplayArch(t *testing.T) {
	cases := map[string]string{"amd64": "x86_64", "arm64": "aarch64", "386": "i386", "riscv64": "riscv64"}
	for in, want := range cases {
		if got := displayArch(in); got != want {
			t.Errorf("displayArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeMountField(t *testing.T) {
	if got := decodeMountField(`/data\040disk`); got != "/data disk" {
		t.Errorf("decoded = %q", got)
	}
	if got := decodeMountField("/plain"); got != "/plain" {
		t.Errorf("decoded = %q", got)
	}
}

func TestCommandFailureDegradesToFieldError(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	ports := readPorts(context.Background())
	if ports.Total != 0 || len(ports.Items) != 0 || ports.Error == "" {
		t.Errorf("ports should degrade to empty + error: %+v", ports)
	}
	systemd := readFailedUnits(context.Background())
	if len(systemd.Failed) != 0 || systemd.Error == "" {
		t.Errorf("systemd should degrade to empty + error: %+v", systemd)
	}
	logins := readLogins(context.Background())
	if len(logins.Items) != 0 || logins.Error == "" {
		t.Errorf("logins should degrade to empty + error: %+v", logins)
	}
}
