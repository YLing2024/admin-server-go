// Package ops 提供受控的运维动作：服务启停、容器重启。
//
// 设计原则（都是为了让「能点」不等于「误点就出事」）：
//   - 目标名必须与 confirm 字段完全一致，前端要用户照抄一遍；
//   - 服务侧有拒绝名单（ssh / 网络 / systemd 自身），这些动作不该从面板触发；
//   - 全局速率限制：3 秒内只允许一次动作，避免连点把服务打死；
//   - 每个动作写审计（谁、什么时候、对什么、结果如何），审计失败不影响动作但会记日志；
//   - 执行命令带超时，绝不无限等待 systemctl/docker。
package ops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 拒绝名单：这些单元从面板重启会让机器失联或打断运维通道。
var serviceDenylist = map[string]bool{
	"ssh.service":              true,
	"sshd.service":             true,
	"networking.service":       true,
	"systemd-networkd.service": true,
	"systemd-resolved.service": true,
	"dbus.service":             true,
	"systemd-journald.service": true,
	"systemd-logind.service":   true,
}

// 允许的动作。
var allowedActions = map[string]bool{
	"start":   true,
	"stop":    true,
	"restart": true,
}

// Request 是一次运维动作请求。
type Request struct {
	Kind    string `json:"kind"`    // service | container
	Name    string `json:"name"`    // 单元名或容器名
	Action  string `json:"action"`  // start | stop | restart
	Confirm string `json:"confirm"` // 必须等于 Name
}

// Result 是执行结果。
type Result struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Action  string `json:"action"`
	Output  string `json:"output,omitempty"`
	Elapsed int64  `json:"elapsedMs"`
}

// Service 执行运维动作并写审计。
type Service struct {
	log       *slog.Logger
	auditPath string

	mu      sync.Mutex
	last    time.Time
	minGap  time.Duration
	timeout time.Duration
}

// New 构造执行器。auditPath 为空时只写日志不落审计文件。
func New(log *slog.Logger, auditPath string) *Service {
	return &Service{
		log:       log,
		auditPath: auditPath,
		minGap:    3 * time.Second,
		timeout:   30 * time.Second,
	}
}

// Execute 校验并执行动作。
func (s *Service) Execute(ctx context.Context, req Request) (Result, error) {
	res := Result{Kind: req.Kind, Name: req.Name, Action: req.Action}
	start := time.Now()

	if !allowedActions[req.Action] {
		return res, fmt.Errorf("不支持的动作: %s", req.Action)
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return res, fmt.Errorf("目标名不能为空")
	}
	if strings.TrimSpace(req.Confirm) != name {
		return res, fmt.Errorf("确认名不匹配：需要原样输入 %q", name)
	}

	switch req.Kind {
	case "service":
		if serviceDenylist[name] {
			return res, fmt.Errorf("%s 在拒绝名单里，不允许从面板操作", name)
		}
		if !strings.HasSuffix(name, ".service") {
			return res, fmt.Errorf("服务名必须以 .service 结尾")
		}
	case "container":
		if strings.ContainsAny(name, " /\\$`;|&") {
			return res, fmt.Errorf("容器名包含非法字符")
		}
	default:
		return res, fmt.Errorf("未知类型: %s", req.Kind)
	}

	s.mu.Lock()
	if time.Since(s.last) < s.minGap {
		s.mu.Unlock()
		return res, fmt.Errorf("操作过于频繁，请等待 %.0f 秒", s.minGap.Seconds())
	}
	s.last = time.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var cmd *exec.Cmd
	if req.Kind == "service" {
		cmd = exec.CommandContext(ctx, "systemctl", req.Action, name)
	} else {
		cmd = exec.CommandContext(ctx, "docker", req.Action, name)
	}
	out, err := cmd.CombinedOutput()
	res.Output = strings.TrimSpace(string(out))
	res.Elapsed = time.Since(start).Milliseconds()
	res.OK = err == nil

	s.audit(req, res, err)
	if err != nil {
		return res, fmt.Errorf("%s %s 失败: %v", req.Kind, req.Action, err)
	}
	return res, nil
}

// audit 追加一行审计；失败只记日志，不影响动作结果。
func (s *Service) audit(req Request, res Result, execErr error) {
	status := "ok"
	if execErr != nil {
		status = "fail"
	}
	line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%dms\t%s\n",
		time.Now().Format(time.RFC3339), req.Kind, req.Action, req.Name, status, res.Elapsed,
		strings.ReplaceAll(res.Output, "\n", " "))
	if s.auditPath != "" {
		f, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		} else if s.log != nil {
			s.log.Warn("运维审计写入失败", "error", err)
		}
	}
	if s.log != nil {
		s.log.Info("运维动作", "kind", req.Kind, "action", req.Action, "name", req.Name, "status", status)
	}
}
