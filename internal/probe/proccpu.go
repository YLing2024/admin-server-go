package probe

import (
	"os"
	"strconv"
	"strings"

	"admin-server-go/internal/util"
)

// CPUEMAAlpha 是统一的 EMA 平滑系数：系统卡片 / 进程排行 / 历史趋势共用同一参数，
// 避免各处的 CPU 曲线抖动幅度不一致。
const CPUEMAAlpha = 0.4

// procCPUresult 是单次 /proc 全量遍历的结果。
type procCPUresult struct {
	perPid   map[int]float64
	totalPct float64
}

// procCPUTicks 读 /proc/<pid>/stat 的已用 CPU 滴答（utime+stime，字段 14/15）。
// 进程名可能含空格，故先定位最后一个 ')'，其后的字段从 state 开始计数。
func procCPUTicks(pid int) (float64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	closeIdx := strings.LastIndex(s, ")")
	if closeIdx == -1 {
		return 0, false
	}
	rest := splitFields(s[closeIdx+1:])
	// rest[0]=state；utime=字段14→rest[11]，stime=字段15→rest[12]
	if len(rest) < 13 {
		return 0, false
	}
	return atof(rest[11]) + atof(rest[12]), true
}

// procTotalTicks 读 /proc/stat 第一行 cpu 的 user+nice+system+idle 之和。
func procTotalTicks() (float64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	first := strings.SplitN(string(data), "\n", 2)[0]
	cols := splitFields(first)
	if len(cols) < 5 || cols[0] != "cpu" {
		return 0, false
	}
	return atof(cols[1]) + atof(cols[2]) + atof(cols[3]) + atof(cols[4]), true
}

// CollectAllProcCpu 单次遍历 /proc 全部进程做差分，perPid 与 totalPct 同源同基准。
// 首次采样（pid 无上一轮状态）无差值，pct 记为 0；进程已退出则清掉采样与平滑缓存。
func (s *Service) CollectAllProcCpu() (map[int]float64, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.collectAllProcCPU()
	return r.perPid, r.totalPct
}

// collectAllProcCPU 是 CollectAllProcCpu 的无锁实现；调用方负责持锁。
func (s *Service) collectAllProcCPU() procCPUresult {
	perPid := map[int]float64{}
	curTotal, ok := procTotalTicks()
	if !ok {
		return procCPUresult{perPid: perPid, totalPct: 0}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return procCPUresult{perPid: perPid, totalPct: 0}
	}

	var sumTicks, dTotal float64
	hasPrev := false
	for _, e := range entries {
		name := e.Name()
		if !isDigits(name) {
			continue
		}
		pid := int(atoi(name))
		curCPU, ok := procCPUTicks(pid)
		if !ok {
			delete(s.procCpuSample, pid)
			delete(s.procCpuEma, pid)
			continue
		}

		pct := 0.0
		if last, seen := s.procCpuSample[pid]; seen && curTotal > last.total {
			dCPU := curCPU - last.cpu
			if dCPU >= 0 {
				sumTicks += dCPU
				if dTotal == 0 {
					dTotal = curTotal - last.total
				}
				pct = (dCPU / dTotal) * 100
				hasPrev = true
			}
		}
		s.procCpuSample[pid] = procSample{cpu: curCPU, total: curTotal}

		var prev *float64
		if v, ok := s.procCpuEma[pid]; ok {
			prev = &v
		}
		smoothed := util.Ema(prev, pct, CPUEMAAlpha)
		s.procCpuEma[pid] = smoothed
		perPid[pid] = round1(smoothed)
	}

	totalPct := 0.0
	if hasPrev && dTotal > 0 {
		var prev *float64
		if s.totalEmaInit {
			v := s.totalEmaPrev
			prev = &v
		}
		smoothed := util.Ema(prev, (sumTicks/dTotal)*100, CPUEMAAlpha)
		s.totalEmaPrev = smoothed
		s.totalEmaInit = true
		totalPct = round1(smoothed)
	}
	return procCPUresult{perPid: perPid, totalPct: totalPct}
}
