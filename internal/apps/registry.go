package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// registryCache 是登记表的 mtime 缓存：命中同 mtime 直接复用解析结果。
type registryCache struct {
	path      string
	mtimeNano int64
	data      map[string]any
}

// readRegistry 读取应用登记表（只读，绝不创建）。返回是否存在、解析后的对象、mtime（毫秒）。
// 解析失败返回错误，message 以「应用登记表解析失败: 」开头。
func (s *Service) readRegistry() (bool, map[string]any, *float64, error) {
	p := s.cfg.AppsFile
	st, err := os.Stat(p)
	if err != nil {
		s.regMu.Lock()
		s.regCache = registryCache{path: p, mtimeNano: -1}
		s.regMu.Unlock()
		return false, nil, nil, nil
	}
	mtimeNano := st.ModTime().UnixNano()

	s.regMu.Lock()
	if s.regCache.path == p && s.regCache.data != nil && s.regCache.mtimeNano == mtimeNano {
		data := s.regCache.data
		s.regMu.Unlock()
		return true, data, mtimeMs(mtimeNano), nil
	}
	s.regMu.Unlock()

	raw, err := os.ReadFile(p)
	if err != nil {
		return false, nil, nil, fmt.Errorf("应用登记表解析失败: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, nil, nil, fmt.Errorf("应用登记表解析失败: %v", err)
	}
	var data map[string]any
	if m, ok := parsed.(map[string]any); ok {
		data = m
	}
	s.regMu.Lock()
	s.regCache = registryCache{path: p, mtimeNano: mtimeNano, data: data}
	s.regMu.Unlock()
	return true, data, mtimeMs(mtimeNano), nil
}

func mtimeMs(nano int64) *float64 {
	v := float64(nano) / 1e6
	return &v
}

// registryPayloadPath 暴露登记表路径（审计与响应字段用，不含内容）。
func (s *Service) registryPath() string { return s.cfg.AppsFile }

// collectOnce 执行一次完整采集（不含 cached 字段；cached 由调用方按命中情况补）。
func (s *Service) collectOnce() (*collectPayload, error) {
	p := s.registryPath()
	exists, data, mtime, err := s.readRegistry() // 解析失败会返回错误，由路由转 500
	if err != nil {
		return nil, err
	}
	generatedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	if !exists {
		return &collectPayload{
			GeneratedAt:   generatedAt,
			TTLSeconds:    ttlSeconds,
			RegistryPath:  p,
			RegistryMtime: nil,
			Notice:        ptrString("未找到应用登记表"),
			Warning:       nil,
			Categories:    []CategoryAgg{},
			Apps:          []AppView{},
			Discovered:    []DiscoveredEntry{},
		}, nil
	}
	if data == nil {
		data = map[string]any{}
	}

	declaredCategories := anyArray(data["categories"])
	rawApps := anyArray(data["apps"])
	ignorePorts := anyArray(data["ignorePorts"])
	ignoreProcesses := anyArray(data["ignoreProcesses"])

	// 校验 + 去重（重复 id 保留第一条并记 warning）。
	seen := map[string]bool{}
	warnings := []string{}
	valid := []map[string]any{}
	for _, item := range rawApps {
		a, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if a["hidden"] == true {
			continue
		}
		idVal := a["id"]
		if jsFalsy(idVal) {
			idVal = ""
		}
		id := jsString(idVal)
		if !idRe.MatchString(id) {
			label := id
			if label == "" {
				label = "(空)"
			}
			warnings = append(warnings, "非法 id: "+label)
			continue
		}
		if seen[id] {
			warnings = append(warnings, "重复 id: "+id)
			continue
		}
		seen[id] = true
		valid = append(valid, a)
	}

	// 并行探活（单条异常不影响其它）。
	results := make([]ProbeResult, len(valid))
	var wg sync.WaitGroup
	for i := range valid {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = probeApp(valid[i], s.tcpProbe)
		}(i)
	}
	wg.Wait()

	apps := make([]AppView, 0, len(valid))
	for i := range valid {
		apps = append(apps, ToAppView(valid[i], results[i]))
	}

	categories, apps := aggregateCategories(declaredCategories, apps)

	occupiedPorts := []any{}
	for _, item := range rawApps {
		a, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if n, ok := jsNumber(a["port"]); ok && n > 0 {
			occupiedPorts = append(occupiedPorts, n)
		}
	}
	discovered := s.collectDiscovered(ignorePorts, ignoreProcesses, occupiedPorts)

	var warning *string
	if len(warnings) > 0 {
		warning = ptrString(strings.Join(warnings, "；"))
	}

	return &collectPayload{
		GeneratedAt:   generatedAt,
		TTLSeconds:    ttlSeconds,
		RegistryPath:  p,
		RegistryMtime: mtime,
		Notice:        nil,
		Warning:       warning,
		Categories:    categories,
		Apps:          apps,
		Discovered:    discovered,
	}, nil
}

// aggregateCategories 分类聚合：未知分类归入「其他」（动态补一个 other），并回写 app.category。
func aggregateCategories(declared []any, apps []AppView) ([]CategoryAgg, []AppView) {
	catMap := map[string]*CategoryAgg{}
	order := []string{}
	for _, c := range declared {
		cm, ok := c.(map[string]any)
		if !ok || !jsTruthy(cm["id"]) {
			continue
		}
		id := jsString(cm["id"])
		name := id
		if jsTruthy(cm["name"]) {
			name = jsString(cm["name"])
		}
		if existing, ok := catMap[id]; ok {
			existing.Name = name
			continue
		}
		catMap[id] = &CategoryAgg{ID: id, Name: name}
		order = append(order, id)
	}
	for i := range apps {
		c, ok := catMap[apps[i].Category]
		if !ok {
			if _, hasOther := catMap[otherCategoryID]; !hasOther {
				catMap[otherCategoryID] = &CategoryAgg{ID: otherCategoryID, Name: otherCategoryName}
				order = append(order, otherCategoryID)
			}
			apps[i].Category = otherCategoryID
			c = catMap[otherCategoryID]
		}
		c.Total++
		if apps[i].Status == StatusUp {
			c.Up++
		} else if apps[i].Status == StatusDown {
			c.Down++
		}
	}
	categories := make([]CategoryAgg, 0, len(order))
	for _, id := range order {
		categories = append(categories, *catMap[id])
	}
	return categories, apps
}

// collectDiscovered 执行 `ss -ltnp` 并解析未登记发现；解析失败返回 []，不影响 apps。
func (s *Service) collectDiscovered(ignorePorts, ignoreProcesses, occupiedPorts []any) []DiscoveredEntry {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ss", "-ltnp")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return []DiscoveredEntry{}
	}
	return ScanDiscovered(stdout.String(), ignorePorts, ignoreProcesses, occupiedPorts)
}

// anyArray 把外部输入规整为数组；非数组返回空数组。
func anyArray(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{}
}

/* ============ 采集器（模块级缓存 + 单飞） ============ */

// collector 持有一次采集的缓存与在途 Promise 等价物；admin 与 public 各自实例，互不影响。
type collector struct {
	s       *Service
	mu      sync.Mutex
	at      time.Time
	payload *collectPayload
	fut     *future
}

// future 是「在途采集」的完成信号与结果，等价旧实现里共享的 Promise。
type future struct {
	done    chan struct{}
	payload *collectPayload
	err     error
}

func newCollector(s *Service) *collector { return &collector{s: s} }

// getPayload 返回采集载荷。命中 10s 缓存且非强制刷新时标记 cached=true；
// 并发请求（含 refresh=1）复用同一次在途采集。
func (c *collector) getPayload(refresh bool) (*collectPayload, error) {
	c.mu.Lock()
	if !refresh && c.payload != nil && time.Since(c.at) < time.Duration(ttlSeconds)*time.Second {
		cp := *c.payload
		cp.Cached = true
		c.mu.Unlock()
		return &cp, nil
	}
	if c.fut != nil {
		f := c.fut
		c.mu.Unlock()
		<-f.done
		if f.err != nil {
			return nil, f.err
		}
		cp := *f.payload
		cp.Cached = false
		return &cp, nil
	}
	f := &future{done: make(chan struct{})}
	c.fut = f
	c.mu.Unlock()

	payload, err := c.s.collectOnce()
	f.payload = payload
	f.err = err

	c.mu.Lock()
	if c.fut == f {
		c.fut = nil
	}
	if err == nil {
		c.payload = payload
		c.at = time.Now()
	}
	c.mu.Unlock()
	close(f.done)

	if err != nil {
		return nil, err
	}
	cp := *payload
	cp.Cached = false
	return &cp, nil
}
