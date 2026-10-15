package apps

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// idRe 是应用 id 的合法形态。
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// NormalizeProbe 推导/规范化探活方式；返回 nil 表示无可用探活（status = unknown）。
func NormalizeProbe(app map[string]any) *Probe {
	p, _ := app["probe"].(map[string]any)
	if p != nil && jsTruthy(p["type"]) {
		switch strings.ToLower(jsString(p["type"])) {
		case "http":
			if jsTruthy(p["target"]) {
				return &Probe{Type: "http", Target: jsString(p["target"]), Expect: parseExpect(p["expect"])}
			}
		case "systemd":
			unit := p["unit"]
			if jsFalsy(unit) {
				unit = app["unit"]
			}
			if jsTruthy(unit) {
				return &Probe{Type: "systemd", Unit: jsString(unit)}
			}
		case "docker":
			container := p["container"]
			if jsFalsy(container) {
				container = app["container"]
			}
			if jsTruthy(container) {
				return &Probe{Type: "docker", Container: jsString(container)}
			}
		case "tcp":
			port := p["port"]
			if jsFalsy(port) {
				port = app["port"]
			}
			if n, ok := jsNumber(port); ok && n > 0 {
				return &Probe{Type: "tcp", Port: int(n)}
			}
		}
	}
	// probe 缺省 / 非法时自动推导。
	if jsTruthy(app["container"]) {
		return &Probe{Type: "docker", Container: jsString(app["container"])}
	}
	if jsTruthy(app["unit"]) {
		return &Probe{Type: "systemd", Unit: jsString(app["unit"])}
	}
	if n, ok := jsNumber(app["port"]); ok && n > 0 {
		return &Probe{Type: "tcp", Port: int(n)}
	}
	return nil
}

// parseExpect 把 expect 数组规整为整数白名单；非数组返回 nil，非有限项丢弃（永不命中）。
func parseExpect(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		if n, ok := jsNumber(item); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// HTTPStatusFromCode 把 HTTP 状态码映射为探活状态：先看 expect 白名单，再按区间归类。
func HTTPStatusFromCode(code int, expect []int) string {
	for _, e := range expect {
		if e == code {
			return StatusUp
		}
	}
	switch {
	case code >= 200 && code < 400:
		return StatusUp
	case code == 401 || code == 403:
		return StatusAuth
	case code >= 500:
		return StatusDegraded
	default:
		return StatusDown
	}
}

// SystemdStatusFromOutput 把 systemctl is-active 输出映射为探活状态。
func SystemdStatusFromOutput(out string) string {
	switch out {
	case "active":
		return StatusUp
	case "activating", "reloading":
		return StatusDegraded
	default:
		return StatusDown
	}
}

// DockerStatusFromOutput 把 docker inspect .State.Status 输出映射为探活状态。
func DockerStatusFromOutput(out string) string {
	switch out {
	case "running":
		return StatusUp
	case "restarting", "paused":
		return StatusDegraded
	default:
		return StatusDown
	}
}

// ToAppView 把登记表条目 + 探活结果归一化成接口返回条目（缺省/异常输入一律兜底）。
func ToAppView(a map[string]any, pr ProbeResult) AppView {
	var port *int
	if n, ok := jsNumber(a["port"]); ok && n > 0 {
		v := int(n)
		port = &v
	}
	nameSrc := a["name"]
	if jsFalsy(nameSrc) {
		nameSrc = a["id"]
	}
	view := AppView{
		ID:        a["id"],
		Name:      truncate(nameSrc, 16),
		Category:  categoryOf(a["category"]),
		Port:      port,
		Tags:      tagsOf(a["tags"]),
		Status:    pr.Status,
		ProbeType: pr.ProbeType,
		LatencyMs: pr.LatencyMs,
		Detail:    pr.Detail,
		Error:     errOrNil(pr.Error),
	}
	if a["desc"] != nil {
		desc := truncate(a["desc"], 24)
		view.Desc = &desc
	}
	if a["onDemand"] == true {
		view.OnDemand = true
	}
	view.URL = strOrNil(a["url"])
	view.Icon = strOrNil(a["icon"])
	return view
}

// categoryOf 复刻 `a.category != null ? String(a.category) : ”`。
func categoryOf(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// tagsOf 复刻 `Array.isArray(tags) ? tags.map(String) : []`。
func tagsOf(v any) []string {
	out := []string{}
	arr, ok := v.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		out = append(out, jsString(item))
	}
	return out
}

var (
	ssLocalPortRe = regexp.MustCompile(`127\.0\.0\.1:(\d+)`)
	ssUserRe      = regexp.MustCompile(`users:\(\("([^"]+)",pid=\d+`)
)

// ScanDiscovered 解析 `ss -ltnp` 输出为未登记发现：只取 127.0.0.1 监听行，
// 排除 ignorePorts / ignoreProcesses / 已登记端口 / docker-proxy，按端口升序最多 30 条。
func ScanDiscovered(stdout string, ignorePorts, ignoreProcesses, occupiedPorts []any) []DiscoveredEntry {
	ignore := numberSet(ignorePorts)
	occupied := numberSet(occupiedPorts)
	procSkip := make([]*regexp.Regexp, 0, len(ignoreProcesses))
	for _, pattern := range ignoreProcesses {
		re, err := regexp.Compile(jsString(pattern))
		if err != nil {
			continue // 单个正则写错不影响其它规则
		}
		procSkip = append(procSkip, re)
	}

	found := map[int]DiscoveredEntry{}
	order := []int{}
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "127.0.0.1:") {
			continue
		}
		pm := ssLocalPortRe.FindStringSubmatch(line)
		if pm == nil {
			continue
		}
		port, err := strconv.Atoi(pm[1])
		if err != nil {
			continue
		}
		if ignore[port] || occupied[port] {
			continue
		}
		var process *string
		if um := ssUserRe.FindStringSubmatch(line); um != nil {
			p := um[1]
			process = &p
		}
		if process != nil && *process == "docker-proxy" {
			continue
		}
		if process != nil {
			skip := false
			for _, re := range procSkip {
				if re.MatchString(*process) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		if _, seen := found[port]; !seen {
			found[port] = DiscoveredEntry{Port: port, Process: process, URL: nil}
			order = append(order, port)
		}
	}
	sort.Ints(order)
	out := make([]DiscoveredEntry, 0, len(order))
	for _, port := range order {
		if len(out) >= maxDiscovered {
			break
		}
		out = append(out, found[port])
	}
	return out
}

// numberSet 把数组按 JS Number 语义规整成整数集合（非有限项丢弃，永不会命中）。
func numberSet(values []any) map[int]bool {
	out := map[int]bool{}
	for _, v := range values {
		if n, ok := jsNumber(v); ok {
			out[int(n)] = true
		}
	}
	return out
}
