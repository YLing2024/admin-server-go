// Package sysinfo 提供「系统信息」区块的只读数据：主机静态概况与实时 Top 进程。
//
// 与 metrics 包的分工：metrics 负责指标留样、趋势与 SSE；本包只回答「这台机器现在是什么状态、
// 在跑什么」，不写库、不动指标采集链路。外部命令仅 `ss` / `systemctl` / `last` 三条只读命令，
// 每条都带 1.5s 超时；任一失败只影响对应字段（返回空数组 + error 文本），接口整体仍为 200。
package sysinfo

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"admin-server-go/internal/httpx"
)

const (
	// infoTTL 是静态概况的缓存时长；进程排行实时采集，不缓存。
	infoTTL = 60 * time.Second
	// commandTimeout 是外部只读命令的超时；三条命令并发执行，整体仍远低于 2s。
	commandTimeout = 1500 * time.Millisecond
	// topLimit 是「按 CPU」「按内存」各取的条数，去重合并后最多 2×topLimit 条。
	topLimit = 10
	// topInterval 是 Top 进程两次采样的间隔（CPU 占比按此区间的滴答差计算）。
	topInterval = 500 * time.Millisecond
)

// Service 是系统信息接口的服务实例。静态概况在进程内缓存 infoTTL。
type Service struct {
	log *slog.Logger

	mu       sync.Mutex
	cached   Info
	cachedAt time.Time
}

// New 构造服务。
func New(log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{log: log}
}

// Info 处理 GET /api/admin/system/info：60s 内复用同一份快照。
// 端口默认全量（需求：监控页要展示全部监听项）；`?ports=15` 保留旧版 15 条上限，
// 切片只作用于本次响应，缓存里始终保存全量。
func (s *Service) Info(w http.ResponseWriter, r *http.Request) {
	info := s.cachedInfo()
	if r.URL.Query().Get("ports") == "15" && len(info.Ports.Items) > 15 {
		info.Ports = Ports{
			Total: info.Ports.Total,
			Items: append([]PortItem(nil), info.Ports.Items[:15]...),
			Error: info.Ports.Error,
		}
	}
	httpx.WriteJSON(w, http.StatusOK, info)
}

// Processes 处理 GET /api/admin/system/processes：实时采样 500ms，不缓存。
// 采集失败不返回 5xx，而是空列表 + error 字段，避免把整个系统页拖垮。
func (s *Service) Processes(w http.ResponseWriter, r *http.Request) {
	processes, err := collectTopProcesses(r.Context(), topLimit, topInterval)
	if err != nil {
		s.log.Warn("Top 进程采集失败", "err", err.Error())
		httpx.WriteJSON(w, http.StatusOK, ProcessesPayload{
			Processes: []TopProcess{},
			Error:     err.Error(),
		})
		return
	}
	if processes == nil {
		processes = []TopProcess{}
	}
	httpx.WriteJSON(w, http.StatusOK, ProcessesPayload{Processes: processes})
}

// cachedInfo 返回缓存内的静态概况；过期后重新采集。持锁采集可避免并发首次请求重复执行命令。
func (s *Service) cachedInfo() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cachedAt.IsZero() && time.Since(s.cachedAt) < infoTTL {
		return s.cached
	}
	info := collectInfo(context.Background())
	s.cached = info
	s.cachedAt = time.Now()
	return info
}
