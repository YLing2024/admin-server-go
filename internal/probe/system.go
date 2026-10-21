package probe

import (
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

/* ============ 原始计数采样（纯读 /proc，不触碰差分状态） ============ */

// CPUSample 返回全部核聚合的 (idle, total) 时钟滴答，供历史采样使用。
// 与实时采样共用 parseCPUStat 的列口径（total 含 user..steal，idle 含 iowait），
// 保证趋势图与系统卡片对得上；历史侧的平滑只发生在 metrics 聚合层。
func CPUSample() (idle, total float64) {
	agg, _, err := readCPUTicks()
	if err != nil {
		return 0, 0
	}
	return agg.idle, agg.total
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

// DiskIOStat 是一块整盘的累计读写字节数（读 / 写各一）。
type DiskIOStat struct {
	ReadBytes  float64
	WriteBytes float64
}

// wholeDiskPattern 匹配整盘设备名：sd[a-z]+ / vd[a-z]+ / hd[a-z]+ / nvme\d+n\d+ /
// mmcblk\d+ / md\d+ / dm-\d+。分区名（vda1、nvme0n1p1、mmcblk0p1、dm-0p1…）因锚定结尾自然不匹配。
var wholeDiskPattern = regexp.MustCompile(`^(?:sd[a-z]+|vd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+|md\d+|dm-\d+)$`)

// isWholeDisk 按设备名判定是否为整盘（而非分区）。旧实现用「minor == 0」判断，
// 对 virtio 的 vdb（minor=16）等会整块漏掉，故一律按名判定。
func isWholeDisk(name string) bool {
	return wholeDiskPattern.MatchString(name)
}

// DiskIOSampleAll 读 /proc/diskstats，按设备名逐块累加整盘的扇区读/写（第 6、10 列）×512，
// 返回「设备名 → 累计读写字节」。分区被排除，避免与整盘重复计数。
func DiskIOSampleAll() map[string]DiskIOStat {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return map[string]DiskIOStat{}
	}
	return parseDiskIOAll(string(data))
}

// parseDiskIOAll 是 DiskIOSampleAll 的纯解析部分，便于以固定夹具做单测。
func parseDiskIOAll(content string) map[string]DiskIOStat {
	out := map[string]DiskIOStat{}
	for _, line := range strings.Split(content, "\n") {
		cols := splitFields(line)
		if len(cols) < 10 {
			continue
		}
		name := cols[2]
		if !isWholeDisk(name) {
			continue
		}
		stat := out[name]
		stat.ReadBytes += atof(cols[5]) * 512
		stat.WriteBytes += atof(cols[9]) * 512
		out[name] = stat
	}
	return out
}

// WholeDiskDevices 列出当前全部整盘设备名（按字典序）。服务启动时探测一次，
// 用于生成 metrics 的按盘速率列；运行中新出现的设备只计入合计，不再新增列。
func WholeDiskDevices() []string {
	data, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return nil
	}
	return wholeDiskDevicesFrom(string(data))
}

// wholeDiskDevicesFrom 是 WholeDiskDevices 的纯解析部分。
func wholeDiskDevicesFrom(content string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		cols := splitFields(line)
		if len(cols) < 3 {
			continue
		}
		if isWholeDisk(cols[2]) {
			seen[cols[2]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DiskIOSample 返回全部整盘的 (读, 写) 累计字节之和（合计口径，供历史合计列与兼容字段使用）。
func DiskIOSample() (read, write float64) {
	total := sumDiskIO(DiskIOSampleAll())
	return total.ReadBytes, total.WriteBytes
}

/* ============ CPU / 负载（只读后台快照） ============ */

// maxCores 限制返回的每核条目数（超出截断；cores 字段仍报真实核数）。
const maxCores = 8

// getCPUInfo 从后台快照组装 CPU 卡片。usage_percent 与 per_core 来自同一对 /proc/stat
// 快照（total 取 cpu 聚合行、per_core 取 cpuN 行），不再做 EMA，也不再因请求触发采样。
// usage_15s / usage_60s 是同一 tick 序列的环形缓冲均值，run_queue / blocked 来自
// 同一个 /proc/stat，top_movers 是该窗口内的增量归因。
func (s *Service) getCPUInfo() map[string]any {
	snap := s.currentHostSnapshot()
	cores := runtime.NumCPU()
	perCore := make([]map[string]any, 0, len(snap.perCore))
	for _, c := range snap.perCore {
		perCore = append(perCore, map[string]any{"id": c.id, "usage_percent": c.percent})
	}
	movers := snap.movers
	if movers == nil {
		movers = []cpuMover{}
	}
	return map[string]any{
		"model":         cpuModel(),
		"usage_percent": snap.cpuPercent,
		"usage_15s":     snap.usage15s,
		"usage_60s":     snap.usage60s,
		"cores":         cores,
		"per_core":      perCore,
		"run_queue":     snap.runQueue,
		"blocked":       snap.blocked,
		"top_movers":    movers,
		"loadavg":       []float64{snap.loadavg[0], snap.loadavg[1], snap.loadavg[2]},
		"load_percent":  snap.loadPercent,
	}
}

/* ============ 系统信息采集 ============ */

// CollectSystem 实时采集系统信息；返回的 map 直接就是 GET /api/admin/system 的响应体。
// CPU / 内存 / 负载只读后台快照，请求路径不推进任何差分基线；其余段失败一律零值兜底。
func (s *Service) CollectSystem() (map[string]any, error) {
	snap := s.currentHostSnapshot()
	mem := snap.mem
	percent := 0.0
	if mem.Total > 0 {
		percent = round1((mem.Used / mem.Total) * 100)
	}
	// 速率只读后台快照（唯一来源）；累计字节与速率同源，避免同一响应内两个时刻混用。
	rates := s.currentRateSnapshot()
	extra := s.currentExtraSnapshot()
	disks := getDisks()
	applyDiskRates(disks, rates)
	applyDiskLatency(disks, extra)
	psi := snap.psi
	if psi == nil {
		psi = emptyPressure()
	}
	services := snap.services
	if services == nil {
		services = []serviceCPU{}
	}
	proc := getProcesses()
	proc["zombie"] = snap.zombie
	netInfo := networkInfo(rates)
	if extra.netOK {
		netInfo["rx_drops"] = extra.netErrors.RxDrops
		netInfo["rx_errors"] = extra.netErrors.RxErrors
		netInfo["tx_drops"] = extra.netErrors.TxDrops
		netInfo["tx_errors"] = extra.netErrors.TxErrors
	}
	if extra.tcp != nil {
		netInfo["tcp"] = map[string]any{
			"established": extra.tcp.Established,
			"time_wait":   extra.tcp.TimeWait,
			"close_wait":  extra.tcp.CloseWait,
			"listen":      extra.tcp.Listen,
			"total":       extra.tcp.Total,
		}
	}
	payload := map[string]any{
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
		"disk":         getDisk(),
		"disks":        disks,
		"network":      netInfo,
		"disk_io":      diskIoInfo(rates),
		"processes":    proc,
		"psi":          psi,
		"top_services": services,
		"stall_score":  snap.stallScore,
		"stall_level":  snap.stallLevel,
		"uptime":       uptimeSeconds(),
		"os":           osType() + " " + osRelease() + " (" + nodeArch() + ")",
		"hostname":     hostname(),
		"containers":   s.containerList(),
	}
	if extra.uptimeOK || extra.fileHandles != nil {
		system := map[string]any{}
		if extra.uptimeOK {
			system["uptime_sec"] = round1(extra.uptimeSec)
		}
		if extra.fileHandles != nil {
			system["file_handles"] = map[string]any{
				"used":    extra.fileHandles.Used,
				"max":     extra.fileHandles.Max,
				"percent": extra.fileHandles.Percent,
			}
		}
		payload["system"] = system
	}
	return payload, nil
}

/* ============ 网络 / 磁盘 I/O 速率（只读快照） ============ */

// networkInfo 从速率快照取网络累计字节与速率；快照未产出时一律为 0（不返回 NaN）。
func networkInfo(snap rateSnapshot) map[string]any {
	return map[string]any{
		"rx_bytes": snap.netBytes.rx,
		"tx_bytes": snap.netBytes.tx,
		"rx_rate":  snap.netRate.rx,
		"tx_rate":  snap.netRate.tx,
	}
}

// diskIoInfo 从速率快照取合计读写累计字节与速率；快照未产出时一律为 0。
func diskIoInfo(snap rateSnapshot) map[string]any {
	return map[string]any{
		"read_bytes":  snap.diskTotalBytes.ReadBytes,
		"write_bytes": snap.diskTotalBytes.WriteBytes,
		"read_rate":   snap.diskTotalRate.ReadBytes,
		"write_rate":  snap.diskTotalRate.WriteBytes,
	}
}

// applyDiskRates 把快照里的按设备累计字节与速率写进 disks[] 每项；
// 设备名缺失或在快照中不存在时保持 0（首次采样速率即为 0）。
func applyDiskRates(disks []map[string]any, snap rateSnapshot) {
	for _, d := range disks {
		dev, _ := d["device"].(string)
		bytes := snap.diskBytes[dev]
		rate := snap.diskRate[dev]
		d["read_rate"] = rate.ReadBytes
		d["write_rate"] = rate.WriteBytes
		d["read_bytes"] = bytes.ReadBytes
		d["write_bytes"] = bytes.WriteBytes
	}
}

// applyDiskLatency 把补充快照里的按盘 IOPS / await 写进 disks[]。
// 快照尚未产出（diskOK=false）时不写这些 key，前端按缺省显示破折号；
// 已产出但某设备本轮没有差分（首次采样）时写 0，不伪造历史值。
func applyDiskLatency(disks []map[string]any, snap extraSnapshot) {
	if !snap.diskOK {
		return
	}
	for _, d := range disks {
		dev, _ := d["device"].(string)
		lat := snap.diskLatency[dev]
		d["read_iops"] = lat.ReadIOPS
		d["write_iops"] = lat.WriteIOPS
		d["await_ms"] = lat.AwaitMS
	}
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
