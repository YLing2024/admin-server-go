package apps

import (
	"regexp"
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

// categoryOf 复刻 `a.category != null ? String(a.category) : ”`。
func categoryOf(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}
