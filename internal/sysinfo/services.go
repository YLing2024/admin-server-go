package sysinfo

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/httpx"
)

// servicesTimeout 是「全部运行中服务」单次采集的硬上限（需求：300ms 内完成）。
// 超时按失败处理，返回空数组而不是半份数据，避免前端把残缺清单当成全量。
const servicesTimeout = 300 * time.Millisecond

// servicesCPUWindow 是两次 cgroup cpu.stat 采样的间隔。取 80ms 是为了在 300ms 预算内
// 留出 systemctl 与文件读取的余量；短窗口的 CPU 占比会抖动，但用于「谁在忙」的排序足够。
const servicesCPUWindow = 80 * time.Millisecond

// RunningService 是 GET /api/admin/system/services 的一行。
// Since 为 epoch 秒（systemd invocation 文件的创建时间），读不到时为 0。
type RunningService struct {
	Unit       string  `json:"unit"`
	Active     string  `json:"active"`
	CPUPercent float64 `json:"cpu_percent"`
	MemMB      *int    `json:"mem_mb"`
	Since      int64   `json:"since"`
}

// unitRecord 是 `systemctl list-units --output=json` 里本项目用到的字段。
type unitRecord struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
}

// cgroupUsage 是一个单元在同一时刻的累计 CPU 与内存读数。
// usageUsec 为 0 且 ok=false 表示该单元没有可读的 cgroup 目录（例如容器外单元）。
type cgroupUsage struct {
	usageUsec uint64
	memMB     *int
	ok        bool
}

// Services 处理 GET /api/admin/system/services：返回全部 running 的 .service 单元。
// 采集失败统一降级为 []（200），只写 warn，不把整个监控页拖成 5xx。
func (s *Service) Services(w http.ResponseWriter, r *http.Request) {
	services, err := collectRunningServices(r.Context())
	if err != nil {
		s.log.Warn("运行中服务采集失败", "err", err.Error())
		httpx.WriteJSON(w, http.StatusOK, []RunningService{})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, services)
}

// collectRunningServices 先取单元清单，再对同一批 cgroup 做两次采样计算 CPU 占比。
// 单元清单失败才返回 error；单个单元的 cgroup 读不到只让该行 CPU=0 / mem=null。
func collectRunningServices(parent context.Context) ([]RunningService, error) {
	ctx, cancel := context.WithTimeout(parent, servicesTimeout)
	defer cancel()

	out, err := runCommandWithin(ctx, servicesTimeout, "systemctl",
		"list-units", "--type=service", "--state=running", "--output=json", "--no-pager")
	if err != nil {
		return nil, err
	}
	units := parseRunningUnits(out)
	if len(units) == 0 {
		return []RunningService{}, nil
	}

	dirs := make([]string, len(units))
	for i, unit := range units {
		dirs[i] = cgroupDir(unit.Unit)
	}

	firstStart := time.Now()
	first := sampleCgroups(ctx, dirs)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if wait := servicesCPUWindow - time.Since(firstStart); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	second := sampleCgroups(ctx, dirs)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	elapsed := time.Since(firstStart)
	cores := runtime.NumCPU()

	rows := make([]RunningService, 0, len(units))
	for i, unit := range units {
		cpu := 0.0
		if first[i].ok && second[i].ok {
			cpu = cpuPercent(first[i].usageUsec, second[i].usageUsec, elapsed, cores)
		}
		rows = append(rows, RunningService{
			Unit:       unit.Unit,
			Active:     unit.Active,
			CPUPercent: cpu,
			MemMB:      second[i].memMB,
			Since:      unitSince(unit.Unit),
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CPUPercent != rows[j].CPUPercent {
			return rows[i].CPUPercent > rows[j].CPUPercent
		}
		return rows[i].Unit < rows[j].Unit
	})
	return rows, nil
}

// parseRunningUnits 解析 systemctl JSON；非法 JSON 或空输出返回空数组（由调用方决定是否告警）。
func parseRunningUnits(text string) []unitRecord {
	var raw []unitRecord
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return []unitRecord{}
	}
	units := make([]unitRecord, 0, len(raw))
	for _, unit := range raw {
		unit.Unit = strings.TrimSpace(unit.Unit)
		if unit.Unit == "" {
			continue
		}
		units = append(units, unit)
	}
	return units
}

// sampleCgroups 并发读取每个单元的 cgroup 目录；返回值与 dirs 一一对应。
func sampleCgroups(ctx context.Context, dirs []string) []cgroupUsage {
	out := make([]cgroupUsage, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		if dir == "" {
			continue
		}
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			out[i] = readCgroupUsage(dir)
		}(i, dir)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
	return out
}

// cgroupDir 返回单元的 cgroup 目录：绝大多数在 system.slice 下；
// 其余（user slice / 容器）用一级通配兜底，避免为了一个单元再调一次 systemctl。
func cgroupDir(unit string) string {
	primary := filepath.Join("/sys/fs/cgroup/system.slice", unit)
	if info, err := os.Stat(primary); err == nil && info.IsDir() {
		return primary
	}
	matches, err := filepath.Glob(filepath.Join("/sys/fs/cgroup", "*", unit))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[0]
}

// readCgroupUsage 读取 cpu.stat 的 usage_usec 与 memory.current；任一缺失仍返回 ok=true，
// 因为 CPU 与内存是两条独立指标，不能因为内存文件缺失就把 CPU 也丢掉。
func readCgroupUsage(dir string) cgroupUsage {
	usage := cgroupUsage{}
	if data, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil {
		usageUsec, ok := parseCPUUsageUsec(string(data))
		if ok {
			usage.usageUsec = usageUsec
			usage.ok = true
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "memory.current")); err == nil {
		usage.memMB = parseMemoryMB(string(data))
	}
	return usage
}

// parseCPUUsageUsec 从 cgroup v2 cpu.stat 取 usage_usec；没有该行返回 ok=false。
func parseCPUUsageUsec(text string) (uint64, bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// parseMemoryMB 把 memory.current 的字节数转成 MB 整数；空 / 非法 / 负数返回 nil（不猜 0）。
func parseMemoryMB(raw string) *int {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	v := int(math.Round(n / 1024 / 1024))
	return &v
}

// cpuPercent 把两次 usage_usec 的差换算成「占整机可用 CPU 时间」的百分比，
// 与 top_services / 进程 CPU 同口径：单核满载约 100 ÷ 核数。
func cpuPercent(prev, cur uint64, elapsed time.Duration, cores int) float64 {
	if elapsed <= 0 || cores <= 0 || cur < prev {
		return 0
	}
	totalUsec := elapsed.Seconds() * float64(cores) * 1e6
	if totalUsec <= 0 {
		return 0
	}
	value := float64(cur-prev) / totalUsec * 100
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return round1(value)
}

// unitSince 用 systemd 的 invocation 符号链接 mtime 作为启动时间（epoch 秒）。
// 该链接在每次启动单元时重建，mtime 即本次启动时刻；文件缺失时返回 0。
func unitSince(unit string) int64 {
	info, err := os.Lstat(filepath.Join("/run/systemd/units", "invocation:"+unit))
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}
