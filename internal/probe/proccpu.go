package probe

import (
	"os"
	"strconv"
	"strings"
)

// procCPUInfo 读 /proc/<pid>/stat 的已用 CPU 滴答（utime+stime，字段 14/15）与进程名。
// 进程名可能含空格，故先定位最后一个 ')'，其后的字段从 state 开始计数。
func procCPUInfo(pid int) (procCPUState, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procCPUState{}, false
	}
	s := string(data)
	openIdx := strings.IndexByte(s, '(')
	closeIdx := strings.LastIndexByte(s, ')')
	if openIdx == -1 || closeIdx <= openIdx {
		return procCPUState{}, false
	}
	rest := splitFields(s[closeIdx+1:])
	// rest[0]=state；utime=字段14→rest[11]，stime=字段15→rest[12]
	if len(rest) < 13 {
		return procCPUState{}, false
	}
	return procCPUState{
		ticks: atof(rest[11]) + atof(rest[12]),
		name:  s[openIdx+1 : closeIdx],
	}, true
}

// CollectAllProcCpu 返回后台采样快照里的「pid → 本窗口 CPU 占比」与系统总占用。
// 进程占比与 usage_percent 同源同窗口，且不再做 EMA；采样器尚未产出时返回空表和 0。
func (s *Service) CollectAllProcCpu() (map[int]float64, float64) {
	snap := s.currentHostSnapshot()
	return cloneProcPct(snap.procCPU), snap.cpuPercent
}
