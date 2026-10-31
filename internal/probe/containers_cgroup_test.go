package probe

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile 造测试文件（目录自动创建）。
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestFillFromCgroup 覆盖 cpu/memory/io/pids 四项的换算，以及两次采样的 CPU% 增量。
func TestFillFromCgroup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker-abc.scope")
	writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec 2000000\nuser_usec 1000000\nsystem_usec 1000000\nnr_periods 0\n")
	writeFile(t, filepath.Join(dir, "memory.current"), "268435456\n") // 256 MB
	writeFile(t, filepath.Join(dir, "memory.max"), "1073741824\n")    // 1 GB
	writeFile(t, filepath.Join(dir, "io.stat"), "8:0 rbytes=1048576 wbytes=2097152 rios=1 wios=2\n")
	writeFile(t, filepath.Join(dir, "pids.current"), "7\n")

	const hostMem = int64(16 * 1024 * 1024 * 1024)
	prev := ctPrevCache{"abc": {usageUsec: 1000000, at: time.Now().Add(-2 * time.Second)}}
	var stat ContainerStat
	sample, ok := fillFromCgroup(dir, "abc", &stat, prev, time.Now(), hostMem)
	if !ok {
		t.Fatal("fillFromCgroup 失败")
	}
	if sample.usageUsec != 2000000 {
		t.Errorf("usageUsec = %d，应为 2000000", sample.usageUsec)
	}
	if stat.MemMB != 256 || stat.MemLimitMB != 1024 || stat.MemPct != 25 {
		t.Errorf("内存换算错误: %+v", stat)
	}
	if stat.BlockReadMB != 1 || stat.BlockWriteMB != 2 {
		t.Errorf("io.stat 换算错误: read=%v write=%v", stat.BlockReadMB, stat.BlockWriteMB)
	}
	if stat.PIDs != 7 {
		t.Errorf("PIDs = %d，应为 7", stat.PIDs)
	}
	// 1 秒 CPU 时间 / 2 秒墙钟 = 50%（1 核 = 100%）
	if stat.CPUPct < 40 || stat.CPUPct > 60 {
		t.Errorf("CPUPct = %v，应接近 50", stat.CPUPct)
	}
}

// TestFillFromCgroupUnlimitedMemory 确认 memory.max=max 时按宿主机总量兜底，而不是 0。
func TestFillFromCgroupUnlimitedMemory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docker-x.scope")
	writeFile(t, filepath.Join(dir, "cpu.stat"), "usage_usec 0\n")
	writeFile(t, filepath.Join(dir, "memory.current"), "1073741824\n") // 1 GB
	writeFile(t, filepath.Join(dir, "memory.max"), "max\n")

	var stat ContainerStat
	if _, ok := fillFromCgroup(dir, "x", &stat, ctPrevCache{}, time.Now(), 16*1024*1024*1024); !ok {
		t.Fatal("fillFromCgroup 失败")
	}
	if stat.MemLimitMB != 16384 {
		t.Errorf("MemLimitMB = %v，应回落到宿主机 16384 MB", stat.MemLimitMB)
	}
	if stat.MemPct < 6 || stat.MemPct > 7 {
		t.Errorf("MemPct = %v，应约 6.25", stat.MemPct)
	}
}

// TestNetTotals 确认跳过 lo、累加非回环接口，并容忍格式不整的行。
func TestNetTotals(t *testing.T) {
	root := t.TempDir()
	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()

	writeFile(t, filepath.Join(root, "4242", "net", "dev"),
		"Inter-|   Receive                                                |  Transmit\n"+
			" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"+
			"    lo: 999999 1 0 0 0 0 0 0 999999 1 0 0 0 0 0 0\n"+
			"  eth0: 1048576 10 0 0 0 0 0 0 2097152 20 0 0 0 0 0 0\n")

	rx, tx := netTotals(4242)
	if rx != 1048576 || tx != 2097152 {
		t.Errorf("netTotals = (%d, %d)，应为 (1048576, 2097152)", rx, tx)
	}
	if rx, tx := netTotals(9999); rx != 0 || tx != 0 {
		t.Errorf("读不到时应为 0，实得 (%d, %d)", rx, tx)
	}
}

// TestContainerCgroupDir 确认两种 cgroup 布局都能命中。
func TestContainerCgroupDir(t *testing.T) {
	root := t.TempDir()
	old := cgroupRoot
	cgroupRoot = root
	defer func() { cgroupRoot = old }()

	if _, ok := containerCgroupDir("missing"); ok {
		t.Error("不存在的容器不该命中")
	}
	writeFile(t, filepath.Join(root, "system.slice", "docker-systemd.scope", "cpu.stat"), "usage_usec 1\n")
	if dir, ok := containerCgroupDir("systemd"); !ok || dir == "" {
		t.Error("system.slice 布局未命中")
	}
	writeFile(t, filepath.Join(root, "docker-cgroupfs.scope", "cpu.stat"), "usage_usec 1\n")
	if _, ok := containerCgroupDir("cgroupfs"); !ok {
		t.Error("cgroupfs 布局未命中")
	}
}
