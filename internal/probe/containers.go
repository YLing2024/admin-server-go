package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// containerCacheTTL 是 docker 采集的缓存时间。`docker stats --no-stream` 单次要
// 数百毫秒到 1 秒，绝不能挂在每个 HTTP/SSE 帧上，因此按 TTL 复用上一次结果。
const containerCacheTTL = 5 * time.Second

// ContainerStat 是一个容器的运行指标。数值字段统一成 MB / 百分比，前端不用再解析字符串。
type ContainerStat struct {
	Name         string  `json:"name"`
	Image        string  `json:"image"`
	State        string  `json:"state"`  // running / exited / paused …
	Status       string  `json:"status"` // docker 的原始状态文本
	CPUPct       float64 `json:"cpu_percent"`
	MemMB        float64 `json:"mem_mb"`
	MemLimitMB   float64 `json:"mem_limit_mb"`
	MemPct       float64 `json:"mem_percent"`
	NetRxMB      float64 `json:"net_rx_mb"`
	NetTxMB      float64 `json:"net_tx_mb"`
	BlockReadMB  float64 `json:"block_read_mb"`
	BlockWriteMB float64 `json:"block_write_mb"`
	PIDs         int     `json:"pids"`
}

// containerCache 保存上一次采集结果与其时间戳；失败时保留旧值，避免面板闪空。
type containerCache struct {
	mu        sync.Mutex
	at        time.Time
	items     []ContainerStat
	errStreak int
}

// Containers 返回缓存的容器指标；缓存过期时同步刷新一次。
// docker 不可用（未安装 / daemon 未起）时返回空切片与错误，由调用方决定如何展示。
func (s *Service) Containers(ctx context.Context) ([]ContainerStat, error) {
	s.containers.mu.Lock()
	defer s.containers.mu.Unlock()

	if time.Since(s.containers.at) < containerCacheTTL {
		return s.containers.items, nil
	}

	items, err := collectContainers(ctx)
	s.containers.at = time.Now()
	if err != nil {
		s.containers.errStreak++
		// 首次失败返回空；之后沿用上一次的成功结果，避免面板在 docker 抖动时闪空。
		if s.containers.items == nil {
			s.containers.items = []ContainerStat{}
		}
		return s.containers.items, err
	}
	s.containers.errStreak = 0
	s.containers.items = items
	return items, nil
}

// dockerPS 是 `docker ps -a --format '{{json .}}'` 的行结构（只取用得到的字段）。
type dockerPS struct {
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	State  string `json:"State"`
	Status string `json:"Status"`
}

// dockerStat 是 `docker stats --no-stream --format '{{json .}}'` 的行结构。
type dockerStat struct {
	Name     string `json:"Name"`
	CPUPerc  string `json:"CPUPerc"`
	MemUsage string `json:"MemUsage"`
	MemPerc  string `json:"MemPerc"`
	NetIO    string `json:"NetIO"`
	BlockIO  string `json:"BlockIO"`
	PIDs     string `json:"PIDs"`
}

// collectContainers 合并 `docker ps -a`（含已停止容器）与 `docker stats`（仅运行中）。
func collectContainers(parent context.Context) ([]ContainerStat, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()

	psOut, err := runDocker(ctx, "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	byName := map[string]*ContainerStat{}
	order := []string{}
	for _, line := range strings.Split(psOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerPS
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		name := strings.TrimPrefix(row.Names, "/")
		if name == "" {
			continue
		}
		byName[name] = &ContainerStat{
			Name:   name,
			Image:  row.Image,
			State:  row.State,
			Status: row.Status,
		}
		order = append(order, name)
	}

	// stats 只对运行中的容器有输出；失败不致命（仍可展示状态列表）。
	statOut, statErr := runDocker(ctx, "stats", "--no-stream", "--format", "{{json .}}")
	if statErr == nil {
		for _, line := range strings.Split(statOut, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row dockerStat
			if json.Unmarshal([]byte(line), &row) != nil {
				continue
			}
			c, ok := byName[row.Name]
			if !ok {
				continue
			}
			c.CPUPct = parsePct(row.CPUPerc)
			c.MemMB, c.MemLimitMB = parsePairMB(row.MemUsage)
			c.MemPct = parsePct(row.MemPerc)
			c.NetRxMB, c.NetTxMB = parsePairMB(row.NetIO)
			c.BlockReadMB, c.BlockWriteMB = parsePairMB(row.BlockIO)
			c.PIDs = parseIntDefault(row.PIDs)
		}
	}

	out := make([]ContainerStat, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

// runDocker 执行 docker CLI，返回 stdout 文本。
func runDocker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// parsePct 解析 "6.47%" → 6.47。
func parsePct(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return round1(v)
}

// parsePairMB 解析 "1.197GiB / 15.62GiB" → (1226.9, 15994.9) MB。
func parsePairMB(s string) (float64, float64) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 0
	}
	return parseSizeMB(parts[0]), parseSizeMB(parts[1])
}

// parseSizeMB 把带单位的体积转成 MB，支持 B/kB/KiB/MB/MiB/GB/GiB/TB。
func parseSizeMB(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	switch unit {
	case "b":
		return num / (1024 * 1024)
	case "kb", "kib":
		return num / 1024
	case "mb", "mib":
		return num
	case "gb", "gib":
		return num * 1024
	case "tb", "tib":
		return num * 1024 * 1024
	}
	return num
}

// parseIntDefault 解析 "101" → 101。
func parseIntDefault(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

// containerList 是给 CollectSystem 用的便捷包装：失败时返回空数组，
// 不让 docker 不可用把整个系统接口拖成 500。
func (s *Service) containerList() []ContainerStat {
	items, err := s.Containers(context.Background())
	if err != nil && s.log != nil {
		s.log.Warn("docker 采集失败，容器列表沿用上次结果", "error", err)
	}
	if items == nil {
		return []ContainerStat{}
	}
	return items
}
