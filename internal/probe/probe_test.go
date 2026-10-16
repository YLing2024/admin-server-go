package probe

import (
	"math"
	"net"
	"testing"

	"admin-server-go/internal/config"
)

// diskstatsFixture / diskstatsFixtureAfter 是两份 /proc/diskstats 夹具：覆盖 virtio 根盘 vda
// （minor=0）、virtio 数据盘 vdb（minor=16，旧判据会漏掉）、各自分区，以及 nvme 盘与其分区。
// After 里 vdb 的写扇区多了 1,228,800（=600MiB），用于速率换算。
const diskstatsFixture = ` 254       0 vda 100 0 200 0 50 0 400 0 0 0 0
 254       1 vda1 50 0 100 0 25 0 200 0 0 0 0
 254      16 vdb 100 0 200 0 50 0 400 0 0 0 0
 254      17 vdb1 50 0 100 0 25 0 200 0 0 0 0
 259       0 nvme0n1 10 0 20 0 5 0 40 0 0 0 0
 259       1 nvme0n1p1 5 0 10 0 2 0 20 0 0 0 0
`

const diskstatsFixtureAfter = ` 254       0 vda 100 0 200 0 50 0 400 0 0 0 0
 254       1 vda1 50 0 100 0 25 0 200 0 0 0 0
 254      16 vdb 100 0 200 0 50 0 1229200 0 0 0 0
 254      17 vdb1 50 0 100 0 25 0 614600 0 0 0 0
 259       0 nvme0n1 10 0 20 0 5 0 40 0 0 0 0
 259       1 nvme0n1p1 5 0 10 0 2 0 20 0 0 0 0
`

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
		"nvme0n1": true, "mmcblk0": true, "md0": true, "dm-0": true,
		"sda1": false, "vda1": false, "nvme0n1p1": false, "mmcblk0p1": false,
		"md0p1": false, "dm-0p1": false, "xda": false, "sd1": false, "": false,
	}
	for in, want := range cases {
		if got := isWholeDisk(in); got != want {
			t.Errorf("isWholeDisk(%q)=%v want %v", in, got, want)
		}
	}
}

// TestDiskIOSampleAllWholeDisks 守住整盘判据：virtio 数据盘 vdb（minor≠0）必须保留，
// 分区（vda1 / vdb1 / nvme0n1p1）全部排除。
func TestDiskIOSampleAllWholeDisks(t *testing.T) {
	got := parseDiskIOAll(diskstatsFixture)
	want := map[string]bool{"vda": true, "vdb": true, "nvme0n1": true}
	if len(got) != len(want) {
		t.Fatalf("整盘集应为 %v，实际 %v", want, got)
	}
	for dev := range want {
		if _, ok := got[dev]; !ok {
			t.Errorf("整盘 %s 被漏掉", dev)
		}
	}
	for _, part := range []string{"vda1", "vdb1", "nvme0n1p1"} {
		if _, ok := got[part]; ok {
			t.Errorf("分区 %s 不应被计入", part)
		}
	}
	if got["vda"].ReadBytes != 200*512 || got["vda"].WriteBytes != 400*512 {
		t.Errorf("vda 累计值错误: %+v", got["vda"])
	}
}

// TestDiskIORatePerDevice 守住按盘速率口径：vdb 10 秒写入增量 600MiB → ~60MiB/s，vda 不串味。
func TestDiskIORatePerDevice(t *testing.T) {
	prev := parseDiskIOAll(diskstatsFixture)
	cur := parseDiskIOAll(diskstatsFixtureAfter)
	rates := computeDiskRates(prev, cur, 10)
	if got := rates["vdb"].WriteBytes; math.Abs(got-60*1024*1024) > 1 {
		t.Fatalf("vdb 写速率=%v，应为 ~60MiB/s", got)
	}
	if rates["vda"].ReadBytes != 0 || rates["vda"].WriteBytes != 0 {
		t.Errorf("vda 不应有速率: %+v", rates["vda"])
	}
}

// TestBaseDevice 守住 filesystem → 基础设备名归一（去 /dev/ 前缀、去分区后缀、mapper 直取）。
func TestBaseDevice(t *testing.T) {
	cases := map[string]string{
		"/dev/vda":        "vda",
		"/dev/vda1":       "vda",
		"/dev/nvme0n1":    "nvme0n1",
		"/dev/nvme0n1p1":  "nvme0n1",
		"/dev/mmcblk0p1":  "mmcblk0",
		"/dev/md0":        "md0",
		"/dev/dm-0":       "dm-0",
		"/dev/mapper/dat": "dat",
	}
	for fs, want := range cases {
		if got := baseDevice(fs); got != want {
			t.Errorf("baseDevice(%q)=%q want %q", fs, got, want)
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
