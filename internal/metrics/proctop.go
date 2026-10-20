package metrics

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"admin-server-go/internal/probe"
)

// DefaultMaxProcessRows 是进程排行的安全上限默认值。
// 全量进程正常规模在数百条，硬上限只兜底极端环境下响应体过大。
const DefaultMaxProcessRows = 2000

// ProcRow 是 ps 输出的一行（内部中间结构）。
type ProcRow struct {
	PID   int     `json:"pid"`
	PPID  int     `json:"ppid"`
	Comm  string  `json:"comm"`
	Args  string  `json:"args"`
	RSSKB int     `json:"rss_kb"`
	MemMB int     `json:"mem_mb"`
	CPU   float64 `json:"cpu"`
}

// ProcNode 是对外响应的一行：自身 RSS / CPU，子树字段为含全部后代的和。
// cgroup_mem_mb 仅当该行是 systemd 单位主进程时为该单位 cgroup 真值，否则 null；
// thread_count 为该进程线程数，读不到为 null（绝不猜 0）。
type ProcNode struct {
	Name         string  `json:"name"`
	PID          int     `json:"pid"`
	PPID         int     `json:"ppid"`
	MemMB        int     `json:"mem_mb"`
	CPU          float64 `json:"cpu"`
	SubtreeMemMB int     `json:"subtree_mem_mb"`
	SubtreeCPU   float64 `json:"subtree_cpu"`
	SubtreeCount int     `json:"subtree_count"`
	CgroupMemMB  *int    `json:"cgroup_mem_mb"`
	ThreadCount  *int    `json:"thread_count"`
}

// procPayload 是进程采集的完整载荷。
type procPayload struct {
	Processes     []ProcNode `json:"processes"`
	Count         int        `json:"count"`
	Truncated     bool       `json:"truncated"`
	TotalMemMB    int64      `json:"total_mem_mb"`
	TotalCPU      float64    `json:"total_cpu"`
	ParsedCount   int        `json:"parsed_count"`
	SkippedCount  int        `json:"skipped_count"`
	TotalCgroupMB *int       `json:"total_cgroup_mb"`
	TotalThreads  int        `json:"total_threads"`
}

// psLineRe 匹配 `pid ppid comm(定宽 16) rss pcpu args`。
// comm 定宽是必须的：进程可能把 comm 改成含空格的名字（如 next-server (v1），
// 纯按空白切分会把列错位、把 RSS 读成 0。comm 之后的 rss/pcpu 为定宽数值列，args 为行尾剩余。
var psLineRe = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(.{16})\s+(\d+)\s+([\d.]+)\s*(.*)$`)

// ParsePsOutputWithStats 解析 ps 文本：空行不计，非空但格式不符的行计入 skipped（不再静默丢弃）。
func ParsePsOutputWithStats(text string) (rows []ProcRow, parsedCount int, skippedCount int) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := psLineRe.FindStringSubmatch(line)
		if m == nil {
			skippedCount++
			continue
		}
		rssKB := int(atoi64(m[4]))
		rows = append(rows, ProcRow{
			PID:   int(atoi64(m[1])),
			PPID:  int(atoi64(m[2])),
			Comm:  strings.TrimRightFunc(m[3], isSpace),
			RSSKB: rssKB,
			MemMB: int(math.Round(float64(rssKB) / 1024)),
			Args:  strings.TrimSpace(m[6]),
			CPU:   math.Round(atof64(m[5])*10) / 10,
		})
	}
	return rows, len(rows), skippedCount
}

// ParsePsOutput 是只返回行数组的兼容入口。
func ParsePsOutput(text string) []ProcRow {
	rows, _, _ := ParsePsOutputWithStats(text)
	return rows
}

var shimIDRe = regexp.MustCompile(`(?:^|\s)--?id\s+([0-9a-fA-F]{6,64})`)

// ExtractShimContainerId 从 containerd shim 的 args 里取 `-id <容器 ID>`（兼容 --id）。
// 手工校验 ID 后一个字符为空白或行尾，等价旧实现的 `(?=\s|$)`（RE2 不支持前瞻）。
func ExtractShimContainerId(args string) *string {
	loc := shimIDRe.FindStringSubmatchIndex(args)
	if loc == nil {
		return nil
	}
	id := args[loc[2]:loc[3]]
	if loc[3] < len(args) {
		c := args[loc[3]]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return nil
		}
	}
	return &id
}

// ParseDockerPs 解析 `docker ps --format '{{.ID}} {{.Names}}'` 文本 → 短 ID → 容器名。
func ParseDockerPs(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		sp := strings.Index(t, " ")
		if sp <= 0 {
			continue
		}
		id := strings.TrimSpace(t[:sp])
		name := strings.TrimSpace(t[sp+1:])
		if id != "" && name != "" {
			out[id] = name
		}
	}
	return out
}

// MatchContainerName 前缀匹配：shim args 通常是 64 位全长 ID，docker ps 给的是 12 位短 ID。
func MatchContainerName(containerID *string, m map[string]string) *string {
	if containerID == nil || len(m) == 0 {
		return nil
	}
	for shortID, name := range m {
		if strings.HasPrefix(*containerID, shortID) {
			n := name
			return &n
		}
	}
	return nil
}

var (
	piRe     = regexp.MustCompile(`(^|/)pi(\.js)?$`)
	pythonRe = regexp.MustCompile(`^python`)
)

// ResolveProcessName 归一化进程名：systemd MainPID → 容器 shim（容器名）→
// opencode / agent-browser / pi / aionui / node / python → comm 兜底。
func ResolveProcessName(row ProcRow, pidToService map[int]string, containerMap map[string]string) string {
	if svc, ok := pidToService[row.PID]; ok {
		return svc
	}
	if strings.HasPrefix(row.Comm, "containerd-shim") {
		if name := MatchContainerName(ExtractShimContainerId(row.Args), containerMap); name != nil {
			return *name
		}
		return row.Comm
	}
	if row.Comm == "opencode" || strings.Contains(row.Args, "opencode") {
		return "opencode"
	}
	if row.Comm == "agent-browser" || strings.Contains(row.Args, "agent-browser") {
		return "agent-browser"
	}
	if row.Comm == "pi" || strings.Contains(row.Args, "pi-coding-agent") || piRe.MatchString(strings.TrimSpace(row.Args)) {
		return "pi"
	}
	if row.Comm == "aioncore" || strings.Contains(row.Args, "aioncore") || strings.Contains(row.Args, "aionui-src") {
		return "aionui"
	}
	if row.Comm == "node" || strings.Contains(row.Args, "node") {
		return "node"
	}
	if pythonRe.MatchString(row.Comm) {
		return "python"
	}
	return row.Comm
}

// SummarizeProcesses 截断 + 合计：count 为截断前总数，total_mem_mb 为全部进程 RSS 之和（MB），
// 按原始 kB 汇总后一次取整（与 `ps -eo rss … | awk '{s+=$1} END{print s/1024}'` 同口径）。
// 超出硬上限按内存降序截断。
func SummarizeProcesses(rows []ProcRow, limit int) (processes []ProcRow, count int, truncated bool, totalMemMB int64) {
	count = len(rows)
	var sumKB int64
	for _, r := range rows {
		sumKB += int64(r.RSSKB)
	}
	totalMemMB = int64(math.Round(float64(sumKB) / 1024))
	truncated = len(rows) > limit
	sorted := append([]ProcRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RSSKB > sorted[j].RSSKB })
	if truncated {
		processes = sorted[:limit]
	} else {
		processes = sorted
	}
	return processes, count, truncated, totalMemMB
}

// BuildProcessTree 构造父子关系并算子树合计：按 ppid 挂到父节点，ppid 不在集合内 / 自环视为根。
// 节点子树合计 = 自身 + 全部后代；subtree_count = 含自身的后代进程数。
// 对异常环路做保护（回边直接跳过），保证绝不无限递归。
func BuildProcessTree(rows []ProcNode) []ProcNode {
	nodes := make([]ProcNode, len(rows))
	copy(nodes, rows)
	for i := range nodes {
		nodes[i].SubtreeMemMB = nodes[i].MemMB
		nodes[i].SubtreeCPU = nodes[i].CPU
		nodes[i].SubtreeCount = 1
	}
	byPid := make(map[int]*ProcNode, len(nodes))
	for i := range nodes {
		byPid[nodes[i].PID] = &nodes[i]
	}
	children := map[int][]int{}
	for i := range nodes {
		n := nodes[i]
		if n.PPID != 0 && n.PPID != n.PID {
			if _, ok := byPid[n.PPID]; ok {
				children[n.PPID] = append(children[n.PPID], n.PID)
			}
		}
	}

	const (
		unvisited  = 0
		inProgress = 1
		done       = 2
	)
	mark := make(map[int]int, len(nodes))
	var visit func(pid int) *ProcNode
	visit = func(pid int) *ProcNode {
		n, ok := byPid[pid]
		if !ok || mark[pid] == inProgress {
			return nil // 环路回边：跳过，不计入
		}
		if mark[pid] == done {
			return n
		}
		mark[pid] = inProgress
		mem := n.MemMB
		cpu := n.CPU
		count := 1
		for _, c := range children[pid] {
			if child := visit(c); child != nil {
				mem += child.SubtreeMemMB
				cpu += child.SubtreeCPU
				count += child.SubtreeCount
			}
		}
		n.SubtreeMemMB = mem
		n.SubtreeCPU = math.Round(cpu*10) / 10
		n.SubtreeCount = count
		mark[pid] = done
		return n
	}
	for i := range nodes {
		visit(nodes[i].PID)
	}
	return nodes
}

// ParseCgroupBytesToMb 把 cgroup v2 memory.current / systemctl MemoryCurrent 的十进制字节
// 转成 MB 整数；空、'[not set]'、'infinity'、负数、超出安全整数的哨兵值一律 null（绝不猜）。
func ParseCgroupBytesToMb(raw string) *int {
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

var unitSuffixRe = regexp.MustCompile(`\.(service|socket|slice|target|mount|timer|path|scope|device)$`)

// ServiceUnitName 规范化 systemd 单元名：裸名补 '.service'，已带已知后缀的保持原样。
func ServiceUnitName(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return s
	}
	if unitSuffixRe.MatchString(s) {
		return s
	}
	return s + ".service"
}

// CgroupMemoryPath 返回单元 cgroup 内存真值文件路径（默认 system.slice）。
func CgroupMemoryPath(name string) string {
	return CgroupMemoryPathIn(name, "system.slice")
}

// CgroupMemoryPathIn 同 CgroupMemoryPath，可指定所属 slice。
// 服务单元 → /sys/fs/cgroup/<slice>/<unit>/memory.current；slice 自身 → /sys/fs/cgroup/<slice>/memory.current。
func CgroupMemoryPathIn(name, slice string) string {
	unit := ServiceUnitName(name)
	if strings.HasSuffix(unit, ".slice") {
		return "/sys/fs/cgroup/" + unit + "/memory.current"
	}
	return "/sys/fs/cgroup/" + slice + "/" + unit + "/memory.current"
}

// ResolveMaxProcessRows 解析 ADMIN_PROC_MAX_ROWS：只接受纯正整数字符串，非法/未设置回退 fallback。
func ResolveMaxProcessRows(raw string, fallback int) int {
	s := strings.TrimSpace(raw)
	if !isAllDigits(s) {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// ParseThreadCount 把 /proc/<pid>/task 的目录条目名转成线程数；只有数字 tid 计入。
// ok=false（目录不存在 / 权限不足 / 进程已退出）返回 null，绝不猜 0。
func ParseThreadCount(entries []string, ok bool) *int {
	if !ok {
		return nil
	}
	n := 0
	for _, e := range entries {
		if isAllDigits(e) {
			n++
		}
	}
	return &n
}

// SumThreads 累加各行 thread_count；nil（读不到）的行跳过，不按 0 计入。
func SumThreads(counts []*int) int {
	n := 0
	for _, c := range counts {
		if c != nil {
			n += *c
		}
	}
	return n
}

/* ============ 采集 ============ */

// collectProcesses 采集全部进程：命名、CPU 覆盖、cgroup/线程归属、截断与构树。
func (s *Service) collectProcesses() (*procPayload, error) {
	// 1. systemd 正规化：逐服务查 MainPID，非 0 记录 pid→服务名映射（并发）。
	pidToService := map[int]string{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, svc := range probe.SystemdServices {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			out, err := execOutput(3*time.Second, "systemctl", "show", svc, "-p", "MainPID", "--value")
			if err != nil {
				return
			}
			pid := int(atoi64(strings.TrimSpace(out)))
			if pid > 0 {
				mu.Lock()
				pidToService[pid] = svc
				mu.Unlock()
			}
		}(svc)
	}
	wg.Wait()

	// 3. 单次遍历 /proc 全部进程差分：perPid 与 totalPct 同源。
	perPid, totalPct := s.probe.CollectAllProcCpu()

	// 4. ps 全量（带 ppid，内存降序）。
	var rows []ProcRow
	parsedCount, skippedCount := 0, 0
	if out, err := execOutput(3*time.Second, "ps", "-eo", "pid=,ppid=,comm:16=,rss=,pcpu=,args=", "--sort=-rss"); err == nil {
		rows, parsedCount, skippedCount = ParsePsOutputWithStats(out)
	}

	// 5. 线程数：覆盖截断前的全量行，使 total_threads 与 `ps -eLf | wc -l` 同口径。
	threadByPid := map[int]*int{}
	var threadCounts []*int
	for _, r := range rows {
		tc := readThreadCount(r.PID)
		threadByPid[r.PID] = tc
		threadCounts = append(threadCounts, tc)
	}
	totalThreads := SumThreads(threadCounts)

	// 6. 仅当存在 containerd shim 时才查 docker（一次，失败静默回退 comm）。
	containerMap := map[string]string{}
	hasShim := false
	for _, r := range rows {
		if strings.HasPrefix(r.Comm, "containerd-shim") {
			hasShim = true
			break
		}
	}
	if hasShim {
		containerMap = collectContainerNames()
	}

	// 7. 截断 + 合计（截断前口径）。
	kept, count, truncated, totalMemMB := SummarizeProcesses(rows, s.procMaxRows)

	// 8. cgroup 真值：只对「主 PID 仍在 kept 里」的单位取；total_cgroup_mb 取 system.slice 整片。
	keptPids := map[int]bool{}
	for _, r := range kept {
		keptPids[r.PID] = true
	}
	type unitEntry struct {
		pid  int
		unit string
	}
	var serviceUnits []unitEntry
	for pid, unit := range pidToService {
		if keptPids[pid] {
			serviceUnits = append(serviceUnits, unitEntry{pid: pid, unit: unit})
		}
	}
	cgroupByPid := map[int]*int{}
	totalCgroup := readCgroupMB("system.slice")
	unitCgroups := make([]*int, len(serviceUnits))
	var wg2 sync.WaitGroup
	for i, u := range serviceUnits {
		wg2.Add(1)
		go func(i int, unit string) {
			defer wg2.Done()
			unitCgroups[i] = readCgroupMB(unit)
		}(i, u.unit)
	}
	wg2.Wait()
	for i, u := range serviceUnits {
		cgroupByPid[u.pid] = unitCgroups[i]
	}

	// 9. 命名 + CPU 覆盖 + cgroup/线程归属 → 构树。
	nodes := make([]ProcNode, 0, len(kept))
	for _, r := range kept {
		cpu := r.CPU
		// 采样器已产出时只用同一窗口的快照值；新进程首帧不在表里按 0，绝不回退到 ps
		// 的「生命周期平均」pcpu（短命进程会因此虚高，破坏排行）。
		if perPid != nil {
			cpu = perPid[r.PID]
		}
		var cgroup *int
		if _, ok := pidToService[r.PID]; ok {
			cgroup = cgroupByPid[r.PID]
		}
		nodes = append(nodes, ProcNode{
			Name:        ResolveProcessName(r, pidToService, containerMap),
			PID:         r.PID,
			PPID:        r.PPID,
			MemMB:       r.MemMB,
			CPU:         cpu,
			CgroupMemMB: cgroup,
			ThreadCount: threadByPid[r.PID],
		})
	}

	return &procPayload{
		Processes:     BuildProcessTree(nodes),
		Count:         count,
		Truncated:     truncated,
		TotalMemMB:    totalMemMB,
		TotalCPU:      totalPct,
		ParsedCount:   parsedCount,
		SkippedCount:  skippedCount,
		TotalCgroupMB: totalCgroup,
		TotalThreads:  totalThreads,
	}, nil
}

// collectContainerNames 一次 docker ps，建「12 位短 ID → 容器名」映射；失败静默返回空 Map。
func collectContainerNames() map[string]string {
	out, err := execOutput(3*time.Second, "docker", "ps", "--format", "{{.ID}} {{.Names}}")
	if err != nil {
		return map[string]string{}
	}
	return ParseDockerPs(out)
}

// readCgroupMB 读一个 systemd 单位的 cgroup 内存真值：优先 cgroup v2 真值文件，
// 回退 `systemctl show <unit> -p MemoryCurrent --value`；任何失败 → null（绝不猜）。
func readCgroupMB(name string) *int {
	if data, err := os.ReadFile(CgroupMemoryPath(name)); err == nil {
		if mb := ParseCgroupBytesToMb(string(data)); mb != nil {
			return mb
		}
	}
	out, err := execOutput(3*time.Second, "systemctl", "show", ServiceUnitName(name), "-p", "MemoryCurrent", "--value")
	if err != nil {
		return nil
	}
	return ParseCgroupBytesToMb(out)
}

// readThreadCount 读 /proc/<pid>/task 的条目数；读不到返回 null（绝不猜 0）。
func readThreadCount(pid int) *int {
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return ParseThreadCount(names, err == nil)
}

/* ============ exec 小工具 ============ */

// execOutput 执行外部命令并返回 stdout；超时用 context 截断（等价旧实现的 timeout 选项）。
func execOutput(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

func isAllDigits(s string) bool {
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

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func atof64(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
