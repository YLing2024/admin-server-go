package probe

// 本文件补齐监控页新增的采集项：运行时长 / 句柄 / TCP 状态 / 网卡丢包与错误 /
// 磁盘 IOPS 与 await。除 sysctl 与 inode（statfs，见 disks.go）外，来源都是 /proc。
//
// 读取失败只让对应字段缺省：每块各自记录 ok，组装响应时按 ok 决定是否写入 key，
// 绝不让单个文件不可读导致整帧 SSE 失败。

import (
	"math"
	"os"
	"strings"
	"time"
)

// fileHandleStat 是 /proc/sys/fs/file-nr 的换算结果。
// used 取「已分配 − 未使用」，max 为系统上限；percent 为一位小数百分比。
type fileHandleStat struct {
	Used    int64
	Max     int64
	Percent float64
}

// tcpStateCount 是 /proc/net/tcp{,6} 按状态码聚合后的计数，total 含全部状态。
type tcpStateCount struct {
	Established int
	TimeWait    int
	CloseWait   int
	Listen      int
	Total       int
}

// netErrorCount 是 /proc/net/dev 第 4/5/11/12 列（RX 错误/丢弃、TX 错误/丢弃）之和。
type netErrorCount struct {
	RxDrops  float64
	RxErrors float64
	TxDrops  float64
	TxErrors float64
}

// diskRaw 是 /proc/diskstats 里算 IOPS / await 所需的累计计数。
type diskRaw struct {
	Reads      float64
	Writes     float64
	ReadTicks  float64
	WriteTicks float64
}

// diskLatency 是一块整盘在一个采样窗口内的 IOPS 与平均 await（毫秒）。
type diskLatency struct {
	ReadIOPS  float64
	WriteIOPS float64
	AwaitMS   float64
}

// extraSnapshot 是后台采样器发布的补充指标快照。
// 各 ok 字段表示来源是否可读；false 时组装响应会省略对应 key，而不是填 0。
type extraSnapshot struct {
	at          time.Time
	uptimeSec   float64
	uptimeOK    bool
	fileHandles *fileHandleStat
	tcp         *tcpStateCount
	netErrors   netErrorCount
	netOK       bool
	diskLatency map[string]diskLatency
	diskOK      bool
}

// extraRaw 是一次原始读取的结果，差分由 sampleExtras 在锁内完成。
type extraRaw struct {
	at          time.Time
	uptimeSec   float64
	uptimeOK    bool
	fileHandles *fileHandleStat
	tcp         *tcpStateCount
	netErrors   netErrorCount
	netOK       bool
	disks       map[string]diskRaw
	diskOK      bool
}

// sampleExtras 采一次补充指标。磁盘 IOPS / await 需要两次 diskstats 做差，
// 因此上一份原始计数由 Service 持有；首次采样只有绝对计数，速率按 0 处理。
func (s *Service) sampleExtras() {
	raw := readExtraRaw()

	s.extraMu.Lock()
	defer s.extraMu.Unlock()

	snap := extraSnapshot{
		at:          raw.at,
		uptimeSec:   raw.uptimeSec,
		uptimeOK:    raw.uptimeOK,
		fileHandles: raw.fileHandles,
		tcp:         raw.tcp,
		netErrors:   raw.netErrors,
		netOK:       raw.netOK,
		diskOK:      raw.diskOK,
	}
	if raw.diskOK {
		dt := 0.0
		if !s.extraPrevAt.IsZero() {
			dt = raw.at.Sub(s.extraPrevAt).Seconds()
		}
		snap.diskLatency = computeDiskLatency(s.extraPrev, raw.disks, dt)
		s.extraPrev = raw.disks
		s.extraPrevAt = raw.at
	}
	s.extraSnap = snap
}

// currentExtraSnapshot 返回最近一份补充指标快照。快照发布后不再被原地修改。
func (s *Service) currentExtraSnapshot() extraSnapshot {
	s.extraMu.RLock()
	defer s.extraMu.RUnlock()
	return s.extraSnap
}

// readExtraRaw 读取全部补充指标的原始计数；每个来源独立失败。
func readExtraRaw() extraRaw {
	raw := extraRaw{at: time.Now()}
	raw.uptimeSec, raw.uptimeOK = readUptime()
	raw.fileHandles = readFileHandles()
	raw.tcp = readTCPStateCounts()
	raw.netErrors, raw.netOK = readNetErrors()
	raw.disks, raw.diskOK = readDiskRaw()
	return raw
}

// readUptime 读 /proc/uptime 第一列（秒）。
func readUptime() (float64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	cols := splitFields(string(data))
	if len(cols) == 0 {
		return 0, false
	}
	return atof(cols[0]), true
}

// readFileHandles 解析 /proc/sys/fs/file-nr。第一列是已分配句柄，第二列是未使用，
// 第三列是上限；used 取两者之差，避免把未使用额度算进占用。
func readFileHandles() *fileHandleStat {
	data, err := os.ReadFile("/proc/sys/fs/file-nr")
	if err != nil {
		return nil
	}
	return parseFileHandles(string(data))
}

// parseFileHandles 是 readFileHandles 的纯解析部分，便于以固定夹具做单测。
func parseFileHandles(content string) *fileHandleStat {
	cols := splitFields(content)
	if len(cols) < 3 {
		return nil
	}
	allocated := atoi(cols[0])
	unused := atoi(cols[1])
	max := atoi(cols[2])
	used := allocated - unused
	if used < 0 {
		used = 0
	}
	percent := 0.0
	if max > 0 {
		percent = round1(float64(used) / float64(max) * 100)
	}
	return &fileHandleStat{Used: used, Max: max, Percent: percent}
}

// readTCPStateCounts 汇总 IPv4 与 IPv6 的 TCP 状态。任一文件可读即视为成功，
// 容器里没有 IPv6 时只统计 IPv4。两份 seq_file 并行读：内核逐连接遍历时互不依赖，
// 串行读会让 1 秒采样窗口多付一次约 1ms 的文件生成开销。
func readTCPStateCounts() *tcpStateCount {
	paths := []string{"/proc/net/tcp", "/proc/net/tcp6"}
	type result struct {
		counts tcpStateCount
		ok     bool
	}
	results := make(chan result, len(paths))
	for _, path := range paths {
		go func(path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				results <- result{}
				return
			}
			counts, _ := parseTCPStates(string(data))
			results <- result{counts: counts, ok: true}
		}(path)
	}
	total := tcpStateCount{}
	ok := false
	for range paths {
		r := <-results
		if !r.ok {
			continue
		}
		ok = true
		total.Established += r.counts.Established
		total.TimeWait += r.counts.TimeWait
		total.CloseWait += r.counts.CloseWait
		total.Listen += r.counts.Listen
		total.Total += r.counts.Total
	}
	if !ok {
		return nil
	}
	return &total
}

// parseTCPStates 解析 /proc/net/tcp 的状态码列。状态码是十六进制：
// 01 ESTABLISHED、06 TIME_WAIT、08 CLOSE_WAIT、0A LISTEN；其余计入 total。
func parseTCPStates(content string) (tcpStateCount, bool) {
	var out tcpStateCount
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) < 2 {
		return out, false
	}
	// 第一行是表头，从第二行起每条连接一行。
	for _, line := range lines[1:] {
		cols := splitFields(line)
		if len(cols) < 4 {
			continue
		}
		out.Total++
		switch strings.ToUpper(cols[3]) {
		case "01":
			out.Established++
		case "06":
			out.TimeWait++
		case "08":
			out.CloseWait++
		case "0A":
			out.Listen++
		}
	}
	return out, true
}

// readNetErrors 汇总 /proc/net/dev 全部接口的 RX/TX 丢包与错误计数（累计值）。
func readNetErrors() (netErrorCount, bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return netErrorCount{}, false
	}
	return parseNetErrors(string(data))
}

// parseNetErrors 是 readNetErrors 的纯解析部分。冒号后的列序为
// bytes packets errs drop ...，TX 从第 9 列开始，故 TX 的 errs/drop 是第 11/12 列。
func parseNetErrors(content string) (netErrorCount, bool) {
	var out netErrorCount
	ok := false
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) <= 2 {
		return out, false
	}
	for _, line := range lines[2:] {
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		parts := splitFields(line[idx+1:])
		if len(parts) < 12 {
			continue
		}
		ok = true
		out.RxErrors += atof(parts[2])
		out.RxDrops += atof(parts[3])
		out.TxErrors += atof(parts[10])
		out.TxDrops += atof(parts[11])
	}
	return out, ok
}

// readDiskRaw 读取全部整盘的累计完成次数与读写耗时（/proc/diskstats）。
func readDiskRaw() (map[string]diskRaw, bool) {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return nil, false
	}
	return parseDiskRaw(string(data)), true
}

// parseDiskRaw 是 readDiskRaw 的纯解析部分，只收整盘，分区不计。
func parseDiskRaw(content string) map[string]diskRaw {
	out := map[string]diskRaw{}
	for _, line := range strings.Split(content, "\n") {
		cols := splitFields(line)
		if len(cols) < 11 {
			continue
		}
		name := cols[2]
		if !isWholeDisk(name) {
			continue
		}
		out[name] = diskRaw{
			Reads:      atof(cols[3]),
			ReadTicks:  atof(cols[6]),
			Writes:     atof(cols[7]),
			WriteTicks: atof(cols[10]),
		}
	}
	return out
}

// computeDiskLatency 用两份累计计数做差：IOPS 为每秒完成次数，await 为
// (读耗时 + 写耗时) ÷ 完成次数，单位为毫秒（diskstats 的耗时列即毫秒）。
func computeDiskLatency(prev, cur map[string]diskRaw, dt float64) map[string]diskLatency {
	out := make(map[string]diskLatency, len(cur))
	if dt <= 0 {
		return out
	}
	for dev, c := range cur {
		p, ok := prev[dev]
		if !ok {
			continue
		}
		dRead := c.Reads - p.Reads
		dWrite := c.Writes - p.Writes
		dTicks := (c.ReadTicks - p.ReadTicks) + (c.WriteTicks - p.WriteTicks)
		dOps := dRead + dWrite
		if dRead < 0 || dWrite < 0 || dTicks < 0 || dOps < 0 {
			// 计数器回绕或设备重建：本窗口不可信，按 0 处理，不输出负数。
			dRead, dWrite, dTicks, dOps = 0, 0, 0, 0
		}
		await := 0.0
		if dOps > 0 {
			await = dTicks / dOps
		}
		out[dev] = diskLatency{
			ReadIOPS:  round1(math.Max(0, dRead) / dt),
			WriteIOPS: round1(math.Max(0, dWrite) / dt),
			AwaitMS:   round1(math.Max(0, await)),
		}
	}
	return out
}
