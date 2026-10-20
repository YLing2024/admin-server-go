package sysinfo

import (
	"context"
	"errors"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TopProcess 是 Top 进程表的一行。CPUPercent 为占整机 CPU 的百分比（非单核口径）。
type TopProcess struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemMB      float64 `json:"memMB"`
	Command    string  `json:"command"`
}

// ProcessesPayload 是 GET /api/admin/system/processes 的响应体。
type ProcessesPayload struct {
	Processes []TopProcess `json:"processes"`
	Error     string       `json:"error,omitempty"`
}

// procSnapshot 是单个进程一次采样的结果。
type procSnapshot struct {
	name    string
	command string
	ticks   float64
	rssKB   int64
}

// collectTopProcesses 两次采样（间隔 interval）计算 CPU 占比；按 CPU、按内存各取 limit 条后去重合并。
// 新出现或已退出的进程不参与本次排序（缺少基准），保证每个 CPU 值都有真实的差分区间。
func collectTopProcesses(ctx context.Context, limit int, interval time.Duration) ([]TopProcess, error) {
	first, totalFirst, err := sampleProcessTable()
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}

	second, totalSecond, err := sampleProcessTable()
	if err != nil {
		return nil, err
	}
	deltaTotal := totalSecond - totalFirst

	rows := make([]TopProcess, 0, len(second))
	for pid, now := range second {
		before, ok := first[pid]
		if !ok {
			continue
		}
		cpu := 0.0
		if deltaTotal > 0 {
			delta := now.ticks - before.ticks
			if delta >= 0 {
				cpu = math.Max(0, math.Min(100, delta/deltaTotal*100))
			}
		}
		rows = append(rows, TopProcess{
			PID:        pid,
			Name:       now.name,
			CPUPercent: round1(cpu),
			MemMB:      round1(float64(now.rssKB) / 1024),
			Command:    now.command,
		})
	}
	return mergeTop(rows, limit), nil
}

// mergeTop 按 CPU 取前 limit、按内存取前 limit，按 pid 去重后合并（CPU 榜在前）。
func mergeTop(rows []TopProcess, limit int) []TopProcess {
	if limit <= 0 {
		return []TopProcess{}
	}
	byCPU := append([]TopProcess(nil), rows...)
	sort.SliceStable(byCPU, func(i, j int) bool {
		if byCPU[i].CPUPercent != byCPU[j].CPUPercent {
			return byCPU[i].CPUPercent > byCPU[j].CPUPercent
		}
		return byCPU[i].PID < byCPU[j].PID
	})
	byMem := append([]TopProcess(nil), rows...)
	sort.SliceStable(byMem, func(i, j int) bool {
		if byMem[i].MemMB != byMem[j].MemMB {
			return byMem[i].MemMB > byMem[j].MemMB
		}
		return byMem[i].PID < byMem[j].PID
	})

	out := make([]TopProcess, 0, limit*2)
	seen := map[int]bool{}
	appendTop := func(list []TopProcess) {
		for i := 0; i < len(list) && i < limit; i++ {
			if seen[list[i].PID] {
				continue
			}
			seen[list[i].PID] = true
			out = append(out, list[i])
		}
	}
	appendTop(byCPU)
	appendTop(byMem)
	return out
}

// sampleProcessTable 一次遍历 /proc：读每个进程的 CPU 滴答、RSS、名称与命令行。
// 读不到的进程（已退出 / 权限不足）直接跳过，不影响整表。
func sampleProcessTable() (map[int]procSnapshot, float64, error) {
	total, ok := readTotalTicks()
	if !ok {
		return nil, 0, errors.New("读取 /proc/stat 失败")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, err
	}
	out := make(map[int]procSnapshot, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		comm, ticks, ok := parseProcStat(string(stat))
		if !ok {
			continue
		}
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil {
			continue
		}
		name, rssKB := parseProcStatus(string(status))
		if name == "" {
			name = comm
		}
		command := readCommandLine("/proc/" + entry.Name() + "/cmdline")
		if command == "" {
			command = name
		}
		out[pid] = procSnapshot{name: name, command: command, ticks: ticks, rssKB: rssKB}
	}
	return out, total, nil
}

// parseProcStat 从 /proc/<pid>/stat 取 comm 与 utime+stime（字段 14/15）。
// comm 可能含空格与括号，故以最后一个 ')' 作为字段边界。
func parseProcStat(content string) (string, float64, bool) {
	open := strings.Index(content, "(")
	closeIdx := strings.LastIndex(content, ")")
	if open < 0 || closeIdx <= open {
		return "", 0, false
	}
	rest := strings.Fields(content[closeIdx+1:])
	if len(rest) < 13 {
		return "", 0, false
	}
	return content[open+1 : closeIdx], parseFloat(rest[11]) + parseFloat(rest[12]), true
}

// parseProcStatus 从 /proc/<pid>/status 取 Name 与 VmRSS（kB）。
func parseProcStatus(content string) (string, int64) {
	name := ""
	var rssKB int64
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "Name":
			name = strings.TrimSpace(value)
		case "VmRSS":
			fields := strings.Fields(value)
			if len(fields) > 0 {
				rssKB = parseInt64(fields[0])
			}
		}
	}
	return name, rssKB
}

// readCommandLine 把 /proc/<pid>/cmdline 的 NUL 分隔参数拼成单行；超过 300 字符截断。
func readCommandLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	command := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
	command = strings.Join(strings.Fields(command), " ")
	return truncate(command, 300)
}

// readTotalTicks 读 /proc/stat 第一行，累加 user..steal 八个字段（guest 已计入 user/nice，不重复加）。
func readTotalTicks() (float64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, false
	}
	total := 0.0
	for _, field := range fields[1:9] {
		total += parseFloat(field)
	}
	return total, true
}
