package apps

import "sync"

// PublicAppView 是公开接口返回的单项应用：字段严格取自白名单（顺序即输出顺序）。
type PublicAppView struct {
	ID        any     `json:"id"`
	Name      string  `json:"name"`
	Category  string  `json:"category"`
	Desc      *string `json:"desc"`
	URL       *string `json:"url"`
	Icon      *string `json:"icon"`
	Status    string  `json:"status"`
	LatencyMs *int64  `json:"latencyMs"`
	OnDemand  bool    `json:"onDemand"`
}

// PublicCategory 是公开接口的分类项：只留 id / name。
type PublicCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PublicPayload 是公开接口的响应体：剔除登记表路径 / mtime / notice / warning 与未登记发现。
type PublicPayload struct {
	Categories []PublicCategory `json:"categories"`
	Apps       []PublicAppView  `json:"apps"`
}

// ToPublicApp 把单条应用投影为公开字段（白名单外全部丢弃）。
func ToPublicApp(app AppView) PublicAppView {
	return PublicAppView{
		ID:        app.ID,
		Name:      app.Name,
		Category:  app.Category,
		Desc:      app.Desc,
		URL:       app.URL,
		Icon:      app.Icon,
		Status:    app.Status,
		LatencyMs: app.LatencyMs,
		OnDemand:  app.OnDemand,
	}
}

// ToPublicPayload 把采集载荷收窄为公开响应：categories 只留 {id,name}，apps 走白名单，
// 剔除 registryPath / registryMtime / notice / warning 与 discovered 等内部信息。
func ToPublicPayload(payload *collectPayload) *PublicPayload {
	cats := make([]PublicCategory, 0, len(payload.Categories))
	for _, c := range payload.Categories {
		cats = append(cats, PublicCategory{ID: c.ID, Name: c.Name})
	}
	apps := make([]PublicAppView, 0, len(payload.Apps))
	for _, a := range payload.Apps {
		apps = append(apps, ToPublicApp(a))
	}
	return &PublicPayload{Categories: cats, Apps: apps}
}

// RefreshLimiter 是按来源 key 的刷新限流：同一 key 在窗口内只允许一次返回 true。
// 公开接口用它挡住「refresh=1 打本机」的放大；窗口内再次请求返回 false，调用方按缓存读。
type RefreshLimiter struct {
	windowMs int64
	mu       sync.Mutex
	last     map[string]int64
}

// NewRefreshLimiter 构造限流器。
func NewRefreshLimiter(windowMs int64) *RefreshLimiter {
	return &RefreshLimiter{windowMs: windowMs, last: map[string]int64{}}
}

// Allow 判断该 key 在 nowMs 时刻能否真正刷新。
func (l *RefreshLimiter) Allow(key string, nowMs int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.last[key]; ok && nowMs-prev < l.windowMs {
		return false
	}
	l.last[key] = nowMs
	// 防 Map 无界增长：超过阈值时清理已过期条目（个人站点访问量下足够）。
	if len(l.last) > 1000 {
		for k, t := range l.last {
			if nowMs-t >= l.windowMs {
				delete(l.last, k)
			}
		}
	}
	return true
}
