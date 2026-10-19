package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// registryCache 是登记表的 mtime 缓存：命中同 mtime 直接复用解析结果。
type registryCache struct {
	path      string
	mtimeNano int64
	data      map[string]any
}

// readRegistry 读取应用登记表（只读，绝不创建）。返回是否存在与解析后的对象。
// 解析失败返回错误，message 以「应用登记表解析失败: 」开头。
func (s *Service) readRegistry() (bool, map[string]any, error) {
	p := s.cfg.AppsFile
	st, err := os.Stat(p)
	if err != nil {
		s.regMu.Lock()
		s.regCache = registryCache{path: p, mtimeNano: -1}
		s.regMu.Unlock()
		return false, nil, nil
	}
	mtimeNano := st.ModTime().UnixNano()

	s.regMu.Lock()
	if s.regCache.path == p && s.regCache.data != nil && s.regCache.mtimeNano == mtimeNano {
		data := s.regCache.data
		s.regMu.Unlock()
		return true, data, nil
	}
	s.regMu.Unlock()

	raw, err := os.ReadFile(p)
	if err != nil {
		return false, nil, fmt.Errorf("应用登记表解析失败: %v", err)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, nil, fmt.Errorf("应用登记表解析失败: %v", err)
	}
	var data map[string]any
	if m, ok := parsed.(map[string]any); ok {
		data = m
	}
	s.regMu.Lock()
	s.regCache = registryCache{path: p, mtimeNano: mtimeNano, data: data}
	s.regMu.Unlock()
	return true, data, nil
}

// toPublicApp 把登记表条目 + 探活结果归一化成公开条目（缺省/异常输入一律兜底）。
//
// 字段严格取自公开白名单；名称截断 16 字、描述截断 24 字，与旧实现一致。
func toPublicApp(a map[string]any, pr ProbeResult) PublicAppView {
	nameSrc := a["name"]
	if jsFalsy(nameSrc) {
		nameSrc = a["id"]
	}
	view := PublicAppView{
		ID:        a["id"],
		Name:      truncate(nameSrc, 16),
		Category:  categoryOf(a["category"]),
		Status:    pr.Status,
		LatencyMs: pr.LatencyMs,
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

// collectOnce 执行一次完整采集：读登记表 → 校验去重 → 并行探活 → 归一化分类。
func (s *Service) collectOnce() (*PublicPayload, error) {
	exists, data, err := s.readRegistry() // 解析失败会返回错误，由公开接口转 503
	if err != nil {
		return nil, err
	}
	if !exists {
		return &PublicPayload{Categories: []PublicCategory{}, Apps: []PublicAppView{}}, nil
	}
	if data == nil {
		data = map[string]any{}
	}

	declaredCategories := anyArray(data["categories"])
	rawApps := anyArray(data["apps"])

	// 校验 + 去重（重复 id 保留第一条；hidden 应用不返回）。
	seen := map[string]bool{}
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
			continue
		}
		if seen[id] {
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

	apps := make([]PublicAppView, 0, len(valid))
	for i := range valid {
		apps = append(apps, toPublicApp(valid[i], results[i]))
	}

	categories, apps := aggregateCategories(declaredCategories, apps)
	return &PublicPayload{Categories: categories, Apps: apps}, nil
}

// aggregateCategories 分类聚合：未知分类归入「其他」（动态补一个 other），并回写 app.category。
// 输出只保留公开字段 id / name，分类顺序为登记表声明顺序（other 追加在末尾）。
func aggregateCategories(declared []any, apps []PublicAppView) ([]PublicCategory, []PublicAppView) {
	names := map[string]string{}
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
		if _, exists := names[id]; exists {
			names[id] = name
			continue
		}
		names[id] = name
		order = append(order, id)
	}
	for i := range apps {
		if _, ok := names[apps[i].Category]; !ok {
			if _, hasOther := names[otherCategoryID]; !hasOther {
				names[otherCategoryID] = otherCategoryName
				order = append(order, otherCategoryID)
			}
			apps[i].Category = otherCategoryID
		}
	}
	categories := make([]PublicCategory, 0, len(order))
	for _, id := range order {
		categories = append(categories, PublicCategory{ID: id, Name: names[id]})
	}
	return categories, apps
}

// anyArray 把外部输入规整为数组；非数组返回空数组。
func anyArray(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{}
}

/* ============ 采集器（模块级缓存 + 单飞） ============ */

// collector 持有一次采集的缓存与在途 Promise 等价物。
type collector struct {
	s       *Service
	mu      sync.Mutex
	at      time.Time
	payload *PublicPayload
	fut     *future
}

// future 是「在途采集」的完成信号与结果，等价旧实现里共享的 Promise。
type future struct {
	done    chan struct{}
	payload *PublicPayload
	err     error
}

func newCollector(s *Service) *collector { return &collector{s: s} }

// getPayload 返回采集载荷。命中 10s 缓存且非强制刷新时复用缓存；
// 并发请求（含 refresh=1）复用同一次在途采集。
func (c *collector) getPayload(refresh bool) (*PublicPayload, error) {
	c.mu.Lock()
	if !refresh && c.payload != nil && time.Since(c.at) < time.Duration(ttlSeconds)*time.Second {
		cp := *c.payload
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
	return &cp, nil
}
