package metrics

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// psLine 构造一行固定列宽的 ps 输出，与 collectProcesses 的 ps 参数一致（comm 定宽 16 列）。
func psLine(pid, ppid int, comm string, rssKb int, pcpu float64, args string) string {
	padded := comm
	for len(padded) < 16 {
		padded += " "
	}
	pcpuStr := strconv.FormatFloat(pcpu, 'f', -1, 64)
	line := fmt.Sprintf("%6d %6d %s %8d %5s %s", pid, ppid, padded, rssKb, pcpuStr, args)
	return strings.TrimRight(line, " ")
}

func i(pid, ppid int, comm string, rssKb, memMb int, cpu float64, args string) ProcRow {
	return ProcRow{PID: pid, PPID: ppid, Comm: comm, Args: args, RSSKB: rssKb, MemMB: memMb, CPU: cpu}
}

func mkRow(pid, rssKb int) ProcRow {
	return ProcRow{PID: pid, Comm: "x", RSSKB: rssKb, MemMB: int(math.Round(float64(rssKb) / 1024))}
}

func iptr(v int) *int { return &v }

func TestParsePsOutput(t *testing.T) {
	sample := strings.Join([]string{
		"  PID   PPID COMMAND             RSS  %CPU COMMAND", // 表头不匹配 → 丢弃
		psLine(1, 0, "systemd", 1024, 0.1, "/sbin/init splash"),
		psLine(123, 1, "node", 51200, 3.5, "node /app/index.js --port 3000"),
		psLine(456, 123, "kworker/0:0", 0, 0.0, ""),
	}, "\n")
	rows := ParsePsOutput(sample)
	if len(rows) != 3 {
		t.Fatalf("rows=%d want 3", len(rows))
	}
	want0 := i(1, 0, "systemd", 1024, 1, 0.1, "/sbin/init splash")
	if !reflect.DeepEqual(rows[0], want0) {
		t.Errorf("rows[0]=%+v want %+v", rows[0], want0)
	}
	if rows[1].MemMB != 50 || rows[1].CPU != 3.5 || rows[1].Args != "node /app/index.js --port 3000" {
		t.Errorf("rows[1]=%+v", rows[1])
	}
	if rows[2].PPID != 123 || rows[2].MemMB != 0 || rows[2].Args != "" {
		t.Errorf("rows[2]=%+v", rows[2])
	}
}

func TestParsePsOutput_CommWithSpaces(t *testing.T) {
	rows := ParsePsOutput(psLine(789, 1, "next-server (v1", 87048, 0.0, "next-server (v16.2.3)"))
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	if rows[0].Comm != "next-server (v1" || rows[0].RSSKB != 87048 || rows[0].MemMB != 85 || rows[0].Args != "next-server (v16.2.3)" {
		t.Errorf("rows[0]=%+v", rows[0])
	}
}

func TestParsePsOutput_InvalidIgnored(t *testing.T) {
	for _, in := range []string{"", "\n\n", "garbage line", "notapid 1 comm 0 0"} {
		if got := ParsePsOutput(in); len(got) != 0 {
			t.Errorf("ParsePsOutput(%q)=%+v want empty", in, got)
		}
	}
}

func TestParsePsOutputWithStats(t *testing.T) {
	sample := strings.Join([]string{
		psLine(1, 0, "systemd", 1024, 0.1, "/sbin/init"),
		"garbage line",
		"",
		"   ",
		psLine(2, 1, "node", 2048, 0.2, "node x"),
	}, "\n")
	rows, parsed, skipped := ParsePsOutputWithStats(sample)
	if len(rows) != 2 || parsed != 2 || skipped != 1 {
		t.Errorf("rows=%d parsed=%d skipped=%d want 2/2/1", len(rows), parsed, skipped)
	}
	rows2, p2, s2 := ParsePsOutputWithStats("\n\n")
	if len(rows2) != 0 || p2 != 0 || s2 != 0 {
		t.Errorf("empty: rows=%d parsed=%d skipped=%d want 0/0/0", len(rows2), p2, s2)
	}
}

func TestSummarizeProcesses(t *testing.T) {
	kept, count, truncated, total := SummarizeProcesses([]ProcRow{mkRow(1, 1024), mkRow(2, 2048)}, DefaultMaxProcessRows)
	if count != 2 || truncated || len(kept) != 2 || total != 3 {
		t.Errorf("count=%d truncated=%v len=%d total=%d want 2/false/2/3", count, truncated, len(kept), total)
	}

	rows := make([]ProcRow, 0, 520)
	for i := 0; i < 520; i++ {
		rows = append(rows, mkRow(i+1, (520-i)*1024))
	}
	kept, count, truncated, total = SummarizeProcesses(rows, 500)
	if count != 520 || !truncated || len(kept) != 500 {
		t.Errorf("count=%d truncated=%v len=%d want 520/true/500", count, truncated, len(kept))
	}
	if kept[0].PID != 1 || kept[499].PID != 500 {
		t.Errorf("kept[0].pid=%d kept[499].pid=%d want 1/500", kept[0].PID, kept[499].PID)
	}
	var sumKB int
	for _, r := range rows {
		sumKB += r.RSSKB
	}
	if want := int64(math.Round(float64(sumKB) / 1024)); total != want {
		t.Errorf("total=%d want %d", total, want)
	}

	// 未排序输入也会按内存降序返回。
	kept, _, _, _ = SummarizeProcesses([]ProcRow{mkRow(1, 1024), mkRow(2, 4096), mkRow(3, 2048)}, DefaultMaxProcessRows)
	gotOrder := []int{kept[0].PID, kept[1].PID, kept[2].PID}
	if !reflect.DeepEqual(gotOrder, []int{2, 3, 1}) {
		t.Errorf("order=%v want [2 3 1]", gotOrder)
	}
}

func node(pid, ppid, memMB int, cpu float64) ProcNode {
	return ProcNode{PID: pid, PPID: ppid, MemMB: memMB, CPU: cpu}
}

func treeByPid(tree []ProcNode) map[int]ProcNode {
	m := map[int]ProcNode{}
	for _, n := range tree {
		m[n.PID] = n
	}
	return m
}

func TestBuildProcessTree(t *testing.T) {
	by := treeByPid(BuildProcessTree([]ProcNode{
		node(1, 0, 100, 1),
		node(2, 1, 20, 0.5),
		node(3, 1, 30, 2),
		node(4, 2, 5, 0.1),
		node(5, 999, 10, 0.3), // 父不在集合内 → 独立根
	}))
	cases := []struct {
		pid   int
		mem   int
		cpu   float64
		count int
	}{
		{1, 155, 3.6, 4},
		{2, 25, 0.6, 2},
		{3, 30, 2, 1},
		{4, 5, 0.1, 1},
		{5, 10, 0.3, 1},
	}
	for _, c := range cases {
		n := by[c.pid]
		if n.SubtreeMemMB != c.mem || n.SubtreeCPU != c.cpu || n.SubtreeCount != c.count {
			t.Errorf("pid %d subtree mem=%d cpu=%v count=%d want %d/%v/%d", c.pid, n.SubtreeMemMB, n.SubtreeCPU, n.SubtreeCount, c.mem, c.cpu, c.count)
		}
	}
}

func TestBuildProcessTree_Cycles(t *testing.T) {
	tree := BuildProcessTree([]ProcNode{
		node(7, 8, 7, 0.7),
		node(8, 7, 8, 0.8),
		node(9, 9, 9, 0.9), // 自环 → 根
	})
	for _, n := range tree {
		if math.IsInf(n.SubtreeCPU, 0) || math.IsNaN(n.SubtreeCPU) {
			t.Errorf("pid %d subtree_cpu not finite: %v", n.PID, n.SubtreeCPU)
		}
	}
	if got := treeByPid(tree)[9].SubtreeMemMB; got != 9 {
		t.Errorf("self-loop subtree=%d want 9", got)
	}
}

func TestExtractShimContainerId(t *testing.T) {
	id := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	get := func(in string) string {
		if p := ExtractShimContainerId(in); p != nil {
			return *p
		}
		return "<nil>"
	}
	if got := get("-namespace moby -address /run/x -id " + id); got != id {
		t.Errorf("dash-id got %q", got)
	}
	if got := get("-namespace moby --id " + id); got != id {
		t.Errorf("double-dash-id got %q", got)
	}
	if got := get("-namespace moby -workdir /x"); got != "<nil>" {
		t.Errorf("no id got %q", got)
	}
	if got := get(""); got != "<nil>" {
		t.Errorf("empty got %q", got)
	}
}

func TestParseDockerPsAndMatch(t *testing.T) {
	m := ParseDockerPs(strings.Join([]string{"abcdef123456 vaultwarden", "001122334455 memos", "", "broken"}, "\n"))
	if len(m) != 2 || m["abcdef123456"] != "vaultwarden" || m["001122334455"] != "memos" {
		t.Fatalf("map=%v", m)
	}
	full := "abcdef123456" + strings.Repeat("0", 52)
	if got := MatchContainerName(&full, m); got == nil || *got != "vaultwarden" {
		t.Errorf("match full=%v", got)
	}
	full2 := "001122334455" + strings.Repeat("f", 52)
	if got := MatchContainerName(&full2, m); got == nil || *got != "memos" {
		t.Errorf("match full2=%v", got)
	}
	dead := "deadbeef00000000"
	if MatchContainerName(&dead, m) != nil {
		t.Error("want nil for unknown id")
	}
	if MatchContainerName(nil, m) != nil {
		t.Error("want nil for nil id")
	}
	if MatchContainerName(&full, nil) != nil {
		t.Error("want nil for nil map")
	}
}

func TestResolveProcessName(t *testing.T) {
	full := "abcdef123456" + strings.Repeat("0", 52)
	row := ProcRow{PID: 42, PPID: 1, Comm: "containerd-shim-runc-v2", Args: "-namespace moby -id " + full}
	cm := ParseDockerPs("abcdef123456 vaultwarden")
	if got := ResolveProcessName(row, map[int]string{}, cm); got != "vaultwarden" {
		t.Errorf("shim got %q", got)
	}
	if got := ResolveProcessName(row, map[int]string{}, map[string]string{}); got != "containerd-shim-runc-v2" {
		t.Errorf("shim fallback got %q", got)
	}
	if got := ResolveProcessName(row, map[int]string{42: "nginx"}, cm); got != "nginx" {
		t.Errorf("MainPID priority got %q", got)
	}

	mk := func(comm, args string) ProcRow { return ProcRow{PID: 1, Comm: comm, Args: args} }
	empty := map[int]string{}
	emptyCM := map[string]string{}
	if got := ResolveProcessName(mk("node", "node x"), empty, emptyCM); got != "node" {
		t.Errorf("node got %q", got)
	}
	if got := ResolveProcessName(mk("python3", ""), empty, emptyCM); got != "python" {
		t.Errorf("python got %q", got)
	}
	if got := ResolveProcessName(mk("opencode", ""), empty, emptyCM); got != "opencode" {
		t.Errorf("opencode got %q", got)
	}
	if got := ResolveProcessName(mk("bash", ""), empty, emptyCM); got != "bash" {
		t.Errorf("bash got %q", got)
	}
}

func TestResolveMaxProcessRows(t *testing.T) {
	if DefaultMaxProcessRows != 2000 {
		t.Fatalf("DefaultMaxProcessRows=%d want 2000", DefaultMaxProcessRows)
	}
	if got := ResolveMaxProcessRows("3000", DefaultMaxProcessRows); got != 3000 {
		t.Errorf("3000 got %d", got)
	}
	if got := ResolveMaxProcessRows(" 42 ", DefaultMaxProcessRows); got != 42 {
		t.Errorf("trim got %d", got)
	}
	for _, bad := range []string{"", "  ", "abc", "0", "-5", "12abc", "3e3", "1.9", "99999999999999999999"} {
		if got := ResolveMaxProcessRows(bad, DefaultMaxProcessRows); got != DefaultMaxProcessRows {
			t.Errorf("bad=%q got %d want %d", bad, got, DefaultMaxProcessRows)
		}
	}
	if got := ResolveMaxProcessRows("abc", 777); got != 777 {
		t.Errorf("custom fallback got %d", got)
	}
}

func TestParseCgroupBytesToMb(t *testing.T) {
	cases := []struct {
		in   string
		want *int
	}{
		{"1048576", iptr(1)},
		{"2621440", iptr(3)},
		{"0", iptr(0)},
		{"", nil},
		{"   ", nil},
		{"[not set]", nil},
		{"infinity", nil},
		{"-1", nil},
		{"18446744073709551615", nil},
	}
	for _, c := range cases {
		got := ParseCgroupBytesToMb(c.in)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("ParseCgroupBytesToMb(%q)=%v want %v", c.in, fmtPtr(got), fmtPtr(c.want))
		}
	}
}

func fmtPtr(p *int) string {
	if p == nil {
		return "<nil>"
	}
	return strconv.Itoa(*p)
}

func TestServiceUnitNameAndCgroupPath(t *testing.T) {
	nameCases := map[string]string{
		"admin-server":  "admin-server.service",
		"redis-server":  "redis-server.service",
		"nginx.service": "nginx.service",
		"system.slice":  "system.slice",
		"":              "",
	}
	for in, want := range nameCases {
		if got := ServiceUnitName(in); got != want {
			t.Errorf("ServiceUnitName(%q)=%q want %q", in, got, want)
		}
	}
	pathCases := map[string]string{
		"admin-server":  "/sys/fs/cgroup/system.slice/admin-server.service/memory.current",
		"nginx.service": "/sys/fs/cgroup/system.slice/nginx.service/memory.current",
		"system.slice":  "/sys/fs/cgroup/system.slice/memory.current",
	}
	for in, want := range pathCases {
		if got := CgroupMemoryPath(in); got != want {
			t.Errorf("CgroupMemoryPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseThreadCount(t *testing.T) {
	cases := []struct {
		in   []string
		ok   bool
		want *int
	}{
		{[]string{"1"}, true, iptr(1)},
		{[]string{"1", "2", "3", "4"}, true, iptr(4)},
		{[]string{"1", "2", "not-a-tid", "", ".", ".."}, true, iptr(2)},
		{[]string{}, true, iptr(0)},
		{[]string{"12", "34"}, true, iptr(2)},
		{nil, false, nil},
		{[]string{"1"}, false, nil},
	}
	for _, c := range cases {
		got := ParseThreadCount(c.in, c.ok)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("ParseThreadCount(%v,%v)=%v want %v", c.in, c.ok, fmtPtr(got), fmtPtr(c.want))
		}
	}
}

func TestSumThreads(t *testing.T) {
	if got := SumThreads([]*int{iptr(1), iptr(3), iptr(10)}); got != 14 {
		t.Errorf("got %d want 14", got)
	}
	if got := SumThreads([]*int{iptr(5), nil, nil}); got != 5 {
		t.Errorf("got %d want 5", got)
	}
	if got := SumThreads(nil); got != 0 {
		t.Errorf("got %d want 0", got)
	}
	if got := SumThreads([]*int{nil}); got != 0 {
		t.Errorf("got %d want 0", got)
	}
}
