package probe

import (
	"math"
	"testing"
	"time"
)

// TestParseFileHandles 守住 file-nr 口径：used = 已分配 − 未使用，percent 一位小数。
func TestParseFileHandles(t *testing.T) {
	got := parseFileHandles("4960 60 10000\n")
	if got == nil {
		t.Fatal("parseFileHandles 返回 nil")
	}
	if got.Used != 4900 || got.Max != 10000 {
		t.Fatalf("used/max = %d/%d，want 4900/10000", got.Used, got.Max)
	}
	if math.Abs(got.Percent-49.0) > 1e-9 {
		t.Fatalf("percent = %v，want 49", got.Percent)
	}
	if parseFileHandles("1 2") != nil {
		t.Error("字段不足时应返回 nil")
	}
}

// TestParseTCPStates 守住状态码聚合：01/06/08/0A 分别计入四项，total 含全部行。
func TestParseTCPStates(t *testing.T) {
	fixture := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 0 10 0
   1: 0100007F:1F90 0100007F:D431 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0 0 10 0
   2: 0100007F:1F90 0100007F:D432 06 00000000:00000000 00:00000000 00000000     0        0 3 1 0 0 10 0
   3: 0100007F:1F90 0100007F:D433 08 00000000:00000000 00:00000000 00000000     0        0 4 1 0 0 10 0
   4: 0100007F:1F90 0100007F:D434 02 00000000:00000000 00:00000000 00000000     0        0 5 1 0 0 10 0
`
	got, ok := parseTCPStates(fixture)
	if !ok {
		t.Fatal("parseTCPStates 应成功")
	}
	want := tcpStateCount{Established: 1, TimeWait: 1, CloseWait: 1, Listen: 1, Total: 5}
	if got != want {
		t.Fatalf("tcp states = %+v，want %+v", got, want)
	}
}

// TestParseNetErrors 守住 /proc/net/dev 列位：RX 取第 3/4 列，TX 取第 11/12 列。
func TestParseNetErrors(t *testing.T) {
	fixture := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 100 2 3 4 0 0 0 0 200 2 5 6 0 0 0 0
  eth0: 10 1 7 8 0 0 0 0 20 1 9 10 0 0 0 0
`
	got, ok := parseNetErrors(fixture)
	if !ok {
		t.Fatal("parseNetErrors 应成功")
	}
	want := netErrorCount{RxErrors: 10, RxDrops: 12, TxErrors: 14, TxDrops: 16}
	if got != want {
		t.Fatalf("net errors = %+v，want %+v", got, want)
	}
}

// TestComputeDiskLatency 守住 IOPS / await 口径：2 秒窗口内 +100 读 / +200 写、
// 读写耗时合计 +200ms → 50 / 100 IOPS，await = 200/300 ≈ 0.7ms。
func TestComputeDiskLatency(t *testing.T) {
	prev := map[string]diskRaw{
		"sda": {Reads: 1000, Writes: 2000, ReadTicks: 500, WriteTicks: 900},
	}
	cur := map[string]diskRaw{
		"sda": {Reads: 1100, Writes: 2200, ReadTicks: 550, WriteTicks: 1050},
	}
	got := computeDiskLatency(prev, cur, 2)
	lat, ok := got["sda"]
	if !ok {
		t.Fatal("缺少 sda 的延迟结果")
	}
	if lat.ReadIOPS != 50 || lat.WriteIOPS != 100 {
		t.Fatalf("IOPS = %v/%v，want 50/100", lat.ReadIOPS, lat.WriteIOPS)
	}
	if math.Abs(lat.AwaitMS-0.7) > 1e-9 {
		t.Fatalf("await = %v，want 0.7", lat.AwaitMS)
	}
	if _, ok := computeDiskLatency(prev, cur, 0)["sda"]; ok {
		t.Error("dt=0 时不应产出差分")
	}
}

// TestParseProcStatZombie 守住僵尸识别：state=Z 计入，普通进程不计。
func TestParseProcStatZombie(t *testing.T) {
	zombie := "123 (worker) Z 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0"
	info, ok := parseProcStat(zombie)
	if !ok || !info.zombie {
		t.Fatalf("parseProcStat(zombie) = %+v ok=%v，want zombie", info, ok)
	}
	running := "124 (worker) R 1 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 12345 0 0"
	info, ok = parseProcStat(running)
	if !ok || info.zombie {
		t.Fatalf("parseProcStat(running) = %+v ok=%v，want 非僵尸", info, ok)
	}
}

// TestCollectExtrasUnder5ms 是需求书「新增采集总耗时 < 5ms」的实测：
// 连续读取全部新增来源，打印平均 / 最大耗时，并用平均值做门禁（单次抖动可能来自宿主机负载）。
func TestCollectExtrasUnder5ms(t *testing.T) {
	const rounds = 200
	var total time.Duration
	var max time.Duration
	for i := 0; i < rounds; i++ {
		start := time.Now()
		raw := readExtraRaw()
		elapsed := time.Since(start)
		total += elapsed
		if elapsed > max {
			max = elapsed
		}
		if !raw.uptimeOK || raw.tcp == nil {
			t.Fatalf("第 %d 轮采集缺关键字段：uptimeOK=%v tcp=%v", i, raw.uptimeOK, raw.tcp)
		}
	}
	avg := total / rounds
	t.Logf("补充指标采集 %d 轮：平均 %s，最大 %s", rounds, avg, max)
	if avg >= 5*time.Millisecond {
		t.Fatalf("平均耗时 %s，超过 5ms", avg)
	}
}
