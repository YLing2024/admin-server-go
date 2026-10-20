package probe

import (
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cgroupSystemSlice 是 systemd 系统服务默认所在的 cgroup v2 路径。
// 只读这个目录下的 *.service，避免遍历整棵 cgroup 树。
const cgroupSystemSlice = "/sys/fs/cgroup/system.slice"

// maxTopServices 是 cgroup CPU 榜的返回上限。
const maxTopServices = 10

// serviceCPU 是某个 systemd 单元在一个 tick 内的 CPU 占比与内存真值。
// CPUPercent 按整机可用 CPU 时间归一（与进程 CPU 同口径，单核满载 ≈ 100/核数）。
type serviceCPU struct {
	Unit       string  `json:"unit"`
	CPUPercent float64 `json:"cpu_percent"`
	MemMB      *int    `json:"mem_mb"`
}

// cgroupUsage 是单个单元在本轮的绝对计数与内存值。
type cgroupUsage struct {
	usageUsec uint64
	memMB     *int
}

// sampleCgroupCPU 读取 system.slice 下全部服务单元的 cpu.stat usage_usec 与
// memory.current，与上一次 tick 做差得到单元级 CPU%。读不到 cpu.stat 的单元
// 直接跳过；首次采样（无基线）CPU 为 0，但内存仍可展示。
func (s *Service) sampleCgroupCPU(now time.Time) []serviceCPU {
	entries, err := os.ReadDir(cgroupSystemSlice)
	if err != nil {
		return nil
	}
	current := make(map[string]cgroupUsage, len(entries))
	rows := make([]serviceCPU, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".service") {
			continue
		}
		dir := cgroupSystemSlice + "/" + entry.Name()
		usage, ok := readUsageUsec(dir + "/cpu.stat")
		if !ok {
			continue
		}
		mem := readCgroupMemMB(dir + "/memory.current")
		current[entry.Name()] = cgroupUsage{usageUsec: usage, memMB: mem}
		rows = append(rows, serviceCPU{Unit: entry.Name(), MemMB: mem})
	}

	s.cgMu.Lock()
	prev := s.cgPrev
	prevAt := s.cgPrevAt
	s.cgPrev = current
	s.cgPrevAt = now
	s.cgMu.Unlock()

	elapsed := now.Sub(prevAt).Seconds()
	cores := float64(runtime.NumCPU())
	if cores < 1 {
		cores = 1
	}
	for i := range rows {
		before, ok := prev[rows[i].Unit]
		if !ok || elapsed <= 0 || current[rows[i].Unit].usageUsec < before.usageUsec {
			continue
		}
		deltaUsec := float64(current[rows[i].Unit].usageUsec - before.usageUsec)
		// usage_usec 是跨核累计的 CPU 时间，除以「墙上时间 × 核数」才与整机口径一致。
		rows[i].CPUPercent = round1(clampPercent(deltaUsec / (elapsed * 1e6 * cores) * 100))
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CPUPercent != rows[j].CPUPercent {
			return rows[i].CPUPercent > rows[j].CPUPercent
		}
		mi, mj := memValue(rows[i].MemMB), memValue(rows[j].MemMB)
		if mi != mj {
			return mi > mj
		}
		return rows[i].Unit < rows[j].Unit
	})
	if len(rows) > maxTopServices {
		rows = rows[:maxTopServices]
	}
	return rows
}

// readUsageUsec 解析 cpu.stat 的 usage_usec；字段缺失或非法返回 ok=false。
func readUsageUsec(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		cols := splitFields(line)
		if len(cols) < 2 || cols[0] != "usage_usec" {
			continue
		}
		v, err := strconv.ParseUint(cols[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// readCgroupMemMB 把 cgroup v2 memory.current 的字节数转成 MB 整数；
// 空值 / 非法值返回 nil，绝不把「读不到」伪装成 0。
func readCgroupMemMB(path string) *int {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseCgroupMemMB(string(data))
}

// parseCgroupMemMB 是 readCgroupMemMB 的纯解析部分，便于单测固定夹具。
func parseCgroupMemMB(raw string) *int {
	s := strings.TrimSpace(raw)
	if s == "" || s == "[not set]" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9007199254740991 {
		return nil
	}
	v := int(math.Round(n / 1024 / 1024))
	return &v
}

// memValue 把可空内存值转成用于排序的数值；nil 视为 0。
func memValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
