package probe

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

/* ============ 原始计数采样（纯读 /proc，不触碰差分状态） ============ */

// CPUSample 返回全部核聚合的 (idle, total) 时钟滴答。
// 口径对齐旧实现 os.cpus()：total 只累加 user/nice/system/idle/irq（不含 iowait/softirq），idle 取 idle。
// 聚合行各列本就是各核之和，故与逐核累加等价。
func CPUSample() (idle, total float64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := splitFields(line)
		nums := parseFields(fields[1:])
		if len(nums) < 5 {
			return 0, 0
		}
		// user, nice, system, idle, iowait, irq, ...
		all := nums[0] + nums[1] + nums[2] + nums[3] + nums[5]
		return nums[3], all
	}
	return 0, 0
}

// NetSample 累加 /proc/net/dev 所有接口的 rx_bytes / tx_bytes。
func NetSample() (rx, tx float64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines[2:] {
		idx := strings.Index(line, ":")
		if idx == -1 {
			continue
		}
		parts := splitFields(line[idx+1:])
		if len(parts) < 9 {
			continue
		}
		rx += atof(parts[0])
		tx += atof(parts[8])
	}
	return rx, tx
}

// DiskIOSample 读 /proc/diskstats，累加主盘设备（sd[a-z]/vd[a-z]/hd[a-z]，不含分区）的
// 扇区读/写（第 6、10 列）×512，得到累计字节。
func DiskIOSample() (read, write float64) {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		cols := splitFields(line)
		if len(cols) < 11 {
			continue
		}
		name := cols[2]
		if !isWholeDisk(name) {
			continue
		}
		// 分区 minor > 0：同一物理盘的分区与整盘会重复计数，只保留整盘。
		if int(atof(cols[1])) != 0 {
			continue
		}
		read += atof(cols[5]) * 512
		write += atof(cols[9]) * 512
	}
	return read, write
}

func isWholeDisk(name string) bool {
	if len(name) != 3 {
		return false
	}
	switch name[0] {
	case 's', 'v', 'h':
	default:
		return false
	}
	if name[1] != 'd' {
		return false
	}
	return name[2] >= 'a' && name[2] <= 'z'
}

/* ============ 每核 CPU 占用 ============ */

// maxCores 限制返回的每核条目数（超出截断；cores 字段仍报真实核数）。
const maxCores = 8

// cpuPerCoreSample 读 /proc/stat 的 cpuN 行，与上次采样做差按 (Δtotal − Δidle)/Δtotal×100 计算。
// idle 口径含 iowait（内核口径，含内核与 IO 等待时间）；读取失败返回空数组，同时保留旧基线不动。
func (s *Service) cpuPerCoreSample() []map[string]any {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return []map[string]any{}
	}
	type core struct {
		id    int
		total float64
		idle  float64
	}
	var cur []core
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu") || strings.HasPrefix(line, "cpu ") {
			continue
		}
		cols := splitFields(line)
		if len(cols) < 9 || !isDigits(cols[0][3:]) {
			continue
		}
		nums := parseFields(cols[1:])
		if len(nums) < 8 {
			continue
		}
		id, err := strconv.Atoi(cols[0][3:])
		if err != nil {
			continue
		}
		cur = append(cur, core{
			id:    id,
			total: nums[0] + nums[1] + nums[2] + nums[3] + nums[4] + nums[5] + nums[6] + nums[7],
			idle:  nums[3] + nums[4],
		})
	}

	cores := make([]map[string]any, 0, maxCores)
	limit := len(cur)
	if limit > maxCores {
		limit = maxCores
	}
	for _, c := range cur[:limit] {
		usage := 0.0
		if prev, ok := s.cpuPerCorePrev[c.id]; ok {
			dt := c.total - prev.total
			if dt > 0 {
				usage = ((dt - (c.idle - prev.idle)) / dt) * 100
			}
		}
		usage = math.Max(0, math.Min(100, usage))
		cores = append(cores, map[string]any{"id": c.id, "usage_percent": round1(usage)})
	}
	// 基线按全量核更新（不只截断后的前 8 个）。
	next := make(map[int]coreTicks, len(cur))
	for _, c := range cur {
		next[c.id] = coreTicks{total: c.total, idle: c.idle}
	}
	s.cpuPerCorePrev = next
	return cores
}

func (s *Service) getCPUInfo() map[string]any {
	return map[string]any{
		"model":         cpuModel(),
		"usage_percent": s.collectAllProcCPU().totalPct,
		"cores":         runtime.NumCPU(),
		"per_core":      s.cpuPerCoreSample(),
		"loadavg":       loadavg(),
	}
}

/* ============ 系统信息采集 ============ */

// CollectSystem 实时采集系统信息；返回的 map 直接就是 GET /api/admin/system 的响应体。
// 失败一律以零值兜底（旧实现同样逐段 try/catch），故 error 恒为 nil。
func (s *Service) CollectSystem() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mem := ReadMeminfo()
	percent := 0.0
	if mem.Total > 0 {
		percent = round1((mem.Used / mem.Total) * 100)
	}
	return map[string]any{
		"cpu": s.getCPUInfo(),
		"memory": map[string]any{
			"total":       mem.Total,
			"used":        mem.Used,
			"free":        mem.Free,
			"percent":     percent,
			"buffCache":   mem.BuffCache,
			"available":   mem.Available,
			"swapTotal":   mem.SwapTotal,
			"swapUsed":    mem.SwapUsed,
			"swapFree":    mem.SwapFree,
			"swapPercent": mem.SwapPercent,
			"zram":        getZram(),
		},
		"disk":      getDisk(),
		"disks":     getDisks(),
		"network":   s.getNetInfo(),
		"disk_io":   s.getDiskIoInfo(),
		"processes": getProcesses(),
		"psi":       readPressure(),
		"uptime":    uptimeSeconds(),
		"os":        osType() + " " + osRelease() + " (" + nodeArch() + ")",
		"hostname":  hostname(),
	}, nil
}

/* ============ 网络 / 磁盘 I/O 速率 ============ */

// getNetInfo 本次与上次采样做差，按时间差换算字节/秒；首次请求速率返回 0。
// 调用方必须已持有 s.mu。
func (s *Service) getNetInfo() map[string]any {
	rx, tx := NetSample()
	now := nowMillis()
	dt := float64(now-s.netState.ts) / 1000
	info := map[string]any{
		"rx_bytes": rx,
		"tx_bytes": tx,
		"rx_rate":  0.0,
		"tx_rate":  0.0,
	}
	if s.netState.ts != 0 && dt > 0 {
		info["rx_rate"] = math.Max(0, (rx-s.netState.rx)/dt)
		info["tx_rate"] = math.Max(0, (tx-s.netState.tx)/dt)
	}
	s.netState = netState{ts: now, rx: rx, tx: tx}
	return info
}

// getDiskIoInfo 同上，返回读写速率（字节/秒）。调用方必须已持有 s.mu。
func (s *Service) getDiskIoInfo() map[string]any {
	read, write := DiskIOSample()
	now := nowMillis()
	dt := float64(now-s.diskioState.ts) / 1000
	info := map[string]any{
		"read_bytes":  read,
		"write_bytes": write,
		"read_rate":   0.0,
		"write_rate":  0.0,
	}
	if s.diskioState.ts != 0 && dt > 0 {
		info["read_rate"] = math.Max(0, (read-s.diskioState.read)/dt)
		info["write_rate"] = math.Max(0, (write-s.diskioState.write)/dt)
	}
	s.diskioState = diskioState{ts: now, read: read, write: write}
	return info
}

/* ============ 进程数 / 主机信息 ============ */

// getProcesses 解析 /proc/loadavg 第 4 列 running/total。
func getProcesses() map[string]any {
	proc := map[string]any{"running": 0, "total": 0}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return proc
	}
	cols := splitFields(strings.TrimSpace(string(data)))
	if len(cols) >= 4 {
		parts := strings.SplitN(cols[3], "/", 2)
		if len(parts) == 2 {
			proc["running"] = int(atoi(parts[0]))
			proc["total"] = int(atoi(parts[1]))
		}
	}
	return proc
}

func loadavg() []float64 {
	out := []float64{0, 0, 0}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out
	}
	cols := splitFields(strings.TrimSpace(string(data)))
	for i := 0; i < 3 && i < len(cols); i++ {
		out[i] = atof(cols[i])
	}
	return out
}

func uptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	cols := splitFields(strings.TrimSpace(string(data)))
	if len(cols) == 0 {
		return 0
	}
	return atof(cols[0])
}

// cpuModel 取 /proc/cpuinfo 的 model name；缺失回退 unknown（旧实现回退 os.cpus()[0].model）。
func cpuModel() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, ":"); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			if key == "model name" || key == "Model" {
				if v := strings.TrimSpace(line[idx+1:]); v != "" {
					return v
				}
			}
		}
	}
	return "unknown"
}

func osType() string    { return readTrimmed("/proc/sys/kernel/ostype", "Linux") }
func osRelease() string { return readTrimmed("/proc/sys/kernel/osrelease", "") }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// nodeArch 把 Go 的 GOARCH 映射为 Node process.arch 的取值，保证 os 字段文本与旧实现一致。
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	default:
		return runtime.GOARCH
	}
}

/* ============ 小工具 ============ */

func splitFields(s string) []string { return strings.Fields(s) }

func parseFields(in []string) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = atof(v)
	}
	return out
}

func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func atoi(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func readTrimmed(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return fallback
	}
	return v
}
