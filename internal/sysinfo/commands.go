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
	"time"
)

// ssProcRe 从 `ss -tlnp` 的 `users:(("name",pid=123,fd=4))` 里取进程名与 pid。
var ssProcRe = regexp.MustCompile(`\("([^"]*)",pid=(\d+)`)

// weekday 用于识别 `last` 省略主机列时以星期开头的日期字段。
var weekday = map[string]bool{
	"Mon": true, "Tue": true, "Wed": true, "Thu": true,
	"Fri": true, "Sat": true, "Sun": true,
}

// readPorts 执行 `ss -tulnp` 并解析全部监听端口（TCP + UDP）；失败返回空数组 + error。
// 不再截断：概况接口按 `?ports=all|15` 在响应层决定是否只给 15 条，缓存里始终是全量。
func readPorts(parent context.Context) Ports {
	out, err := runCommand(parent, "ss", "-tulnp")
	if err != nil {
		return Ports{Items: []PortItem{}, Error: err.Error()}
	}
	items := parseSSOutput(out)
	return Ports{Total: len(items), Items: items}
}

// parseSSOutput 解析 `ss` 的 socket 列表：兼容 `ss -tulnp`（带 Netid 列）与旧 `ss -tlnp`
// （无 Netid 列，所有行都是 TCP LISTEN）。只收 LISTEN / UNCONN 行，按端口升序（同端口按协议、进程名）。
// 地址取本地地址列（`0.0.0.0:22` / `[::]:443` / `*:80`）。
func parseSSOutput(text string) []PortItem {
	items := []PortItem{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		protocol, state, local := "", "", ""
		switch {
		case fields[0] == "tcp" || fields[0] == "udp":
			if len(fields) < 5 {
				continue
			}
			protocol, state, local = fields[0], fields[1], fields[4]
		case fields[0] == "LISTEN" || fields[0] == "UNCONN":
			if len(fields) < 4 {
				continue
			}
			protocol, state, local = "tcp", fields[0], fields[3]
		default:
			continue
		}
		if state != "LISTEN" && state != "UNCONN" {
			continue
		}
		address, port := splitLocalAddress(local)
		if port <= 0 {
			continue
		}
		item := PortItem{Port: port, Protocol: protocol, State: state, Address: address}
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
		if items[i].Protocol != items[j].Protocol {
			return items[i].Protocol < items[j].Protocol
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

// readLogins 优先执行 `last -n 5 -w`；本机若没有 last/wtmp（精简镜像常见），
// 回退到 journald 的 sshd 记录，避免该卡片长期显示「采集失败」。
func readLogins(parent context.Context) Logins {
	out, err := runCommand(parent, "last", "-n", "5", "-w")
	if err == nil {
		if items := parseLastOutput(out); len(items) > 0 {
			return Logins{Items: items}
		}
	}
	jout, jerr := runCommand(parent, "journalctl", "-u", "ssh", "-u", "sshd",
		"--since", "7 days ago", "-o", "short-iso", "--no-pager")
	if jerr != nil {
		if err != nil {
			return Logins{Items: []LoginItem{}, Error: err.Error()}
		}
		return Logins{Items: []LoginItem{}, Error: jerr.Error()}
	}
	return Logins{Items: parseSSHJournal(jout)}
}

// sshAcceptedRe 匹配 journald 的 sshd 成功登录行：
// `2026-10-20T23:05:15+08:00 host sshd-session[1]: Accepted publickey for root from 1.2.3.4 port 5 ssh2`
var sshAcceptedRe = regexp.MustCompile(`^(\S+)\s+\S+\s+\S+:\s+Accepted\s+\S+\s+for\s+(\S+)\s+from\s+(\S+)\s+port\s+(\d+)`)

// parseSSHJournal 从 journalctl 输出里提取成功登录，按时间倒序保留最近 5 条。
// Time 用 ISO 时间戳，Time 文本与 `last` 分支不同属预期（前端只当字符串展示）。
func parseSSHJournal(text string) []LoginItem {
	items := []LoginItem{}
	for _, line := range strings.Split(text, "\n") {
		m := sshAcceptedRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, LoginItem{Time: m[1], User: m[2], IP: m[3]})
	}
	if len(items) > 5 {
		items = items[len(items)-5:]
	}
	// journal 输出是旧的在前，这里翻成新的在前。
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
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
	return runCommandWithin(parent, commandTimeout, name, args...)
}

// runCommandWithin 与 runCommand 相同，但允许调用方指定更短超时（服务清单要求 300ms 内完成）。
func runCommandWithin(parent context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
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
		detail = "执行超时（" + timeout.String() + "）"
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
