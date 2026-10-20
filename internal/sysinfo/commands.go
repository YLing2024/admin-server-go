package sysinfo

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ssProcRe 从 `ss -tlnp` 的 `users:(("name",pid=123,fd=4))` 里取进程名与 pid。
var ssProcRe = regexp.MustCompile(`\("([^"]*)",pid=(\d+)`)

// weekday 用于识别 `last` 省略主机列时以星期开头的日期字段。
var weekday = map[string]bool{
	"Mon": true, "Tue": true, "Wed": true, "Thu": true,
	"Fri": true, "Sat": true, "Sun": true,
}

// readPorts 执行 `ss -tlnp` 并解析监听端口；失败返回空数组 + error。
func readPorts(parent context.Context) Ports {
	out, err := runCommand(parent, "ss", "-tlnp")
	if err != nil {
		return Ports{Items: []PortItem{}, Error: err.Error()}
	}
	items := parseSSOutput(out)
	total := len(items)
	if len(items) > 15 {
		items = items[:15]
	}
	return Ports{Total: total, Items: items}
}

// parseSSOutput 解析 `ss -tlnp`：只收 LISTEN 行，按端口升序（同端口按进程名）。
// 地址取本地地址列（`0.0.0.0:22` / `[::]:443` / `*:80`）。
func parseSSOutput(text string) []PortItem {
	items := []PortItem{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "LISTEN" {
			continue
		}
		address, port := splitLocalAddress(fields[3])
		if port <= 0 {
			continue
		}
		item := PortItem{Port: port, Address: address}
		if m := ssProcRe.FindStringSubmatch(line); m != nil {
			item.Process = m[1]
			if pid, err := strconv.Atoi(m[2]); err == nil {
				item.PID = &pid
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Port != items[j].Port {
			return items[i].Port < items[j].Port
		}
		return items[i].Process < items[j].Process
	})
	return items
}

// splitLocalAddress 拆出监听地址与端口；解析不出正整数端口时返回 0。
func splitLocalAddress(local string) (string, int) {
	idx := strings.LastIndex(local, ":")
	if idx < 0 || idx == len(local)-1 {
		return local, 0
	}
	port, err := strconv.Atoi(local[idx+1:])
	if err != nil || port <= 0 {
		return local[:idx], 0
	}
	return local[:idx], port
}

// readFailedUnits 执行 `systemctl --failed --no-legend --plain`；失败返回空数组 + error。
func readFailedUnits(parent context.Context) Systemd {
	out, err := runCommand(parent, "systemctl", "--failed", "--no-legend", "--plain")
	if err != nil {
		return Systemd{Failed: []FailedUnit{}, Error: err.Error()}
	}
	return Systemd{Failed: parseFailedUnits(out)}
}

// parseFailedUnits 解析 systemctl 的定宽表：UNIT LOAD ACTIVE SUB DESCRIPTION。
// 描述可能含空格，取第 5 列起的剩余部分。
func parseFailedUnits(text string) []FailedUnit {
	units := []FailedUnit{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := FailedUnit{Name: fields[0]}
		if len(fields) > 4 {
			unit.Description = strings.Join(fields[4:], " ")
		}
		units = append(units, unit)
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units
}

// readLogins 执行 `last -n 5 -w`；失败返回空数组 + error。
// 只保留真实用户登录，过滤 reboot/shutdown/wtmp 这类伪记录。
func readLogins(parent context.Context) Logins {
	out, err := runCommand(parent, "last", "-n", "5", "-w")
	if err != nil {
		return Logins{Items: []LoginItem{}, Error: err.Error()}
	}
	return Logins{Items: parseLastOutput(out)}
}

// parseLastOutput 解析 `last -w`：用户 / 终端 / 主机 / 时间（原始文本）。
// 主机列缺失（下一列直接是星期）时 ip 留空；`-` 与 `:0` 这类非网络来源也留空。
func parseLastOutput(text string) []LoginItem {
	items := []LoginItem{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		user := fields[0]
		if user == "reboot" || user == "shutdown" || user == "wtmp" || user == "btmp" {
			continue
		}
		item := LoginItem{User: user}
		timeStart := 3
		if weekday[fields[2]] {
			timeStart = 2
		} else if fields[2] != "-" && !strings.HasPrefix(fields[2], ":") {
			item.IP = fields[2]
		}
		item.Time = strings.Join(fields[timeStart:], " ")
		items = append(items, item)
		if len(items) == 5 {
			break
		}
	}
	return items
}

// runCommand 在 1.5s 超时内执行只读命令；错误文本带上 stderr，便于接口字段直接展示。
func runCommand(parent context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	if ctx.Err() == context.DeadlineExceeded {
		detail = "执行超时（" + commandTimeout.String() + "）"
	}
	return string(out), fmt.Errorf("%s: %s", name, truncate(detail, 200))
}

// truncate 按字符截断，避免把命令的整段 stderr 塞进响应。
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
