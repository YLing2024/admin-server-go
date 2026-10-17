// Package apps 实现应用面板：只读消费应用登记表 + 实时探活。
//
// 数据源是应用登记表（ADMIN_APPS_FILE，cfg.AppsFile）。只读消费，文件不存在不自动生成；
// mtime 变化即重读（改登记表无需重启服务）。探活优先级 probe > container > unit > port(tcp) > unknown。
//
// 口径逐项对齐旧实现 src/apps.ts 与 src/routes/apps.ts + src/routes/public-apps.ts。
package apps

import (
	"fmt"
	"strconv"
	"strings"
)

// 探活状态语义：
//
//	up       http 2xx/3xx 或 expect 命中 / systemd active / docker running / tcp 通
//	auth     http 401 / 403
//	degraded http 5xx 或超时 / systemd activating|reloading / docker restarting|paused
//	down     连接被拒 / 解析失败 / systemd 非 active / docker exited|dead
//	idle     按需唤醒应用（onDemand:true）当前探活失败 —— 属正常休眠，非故障
//	unknown  没有可用的探活方式
const (
	StatusUp       = "up"
	StatusAuth     = "auth"
	StatusDegraded = "degraded"
	StatusDown     = "down"
	StatusIdle     = "idle"
	StatusUnknown  = "unknown"
)

// 缓存与探活参数。
const (
	ttlSeconds        = 10
	probeTimeoutMs    = 1500
	maxDiscovered     = 30
	otherCategoryID   = "other"
	otherCategoryName = "其他"
	refreshWindowMs   = 10000
)

// Probe 是规范化后的探活方式。
type Probe struct {
	Type      string
	Target    string
	Expect    []int
	Unit      string
	Container string
	Port      int
}

// ProbeResult 是单条探活结果（含命中的探活方式）。
type ProbeResult struct {
	Status    string
	ProbeType *string
	LatencyMs *int64
	Detail    *string
	Error     *string
}

// AppView 是接口返回的应用条目（字段名严格固定）。
type AppView struct {
	ID        any      `json:"id"`
	Name      string   `json:"name"`
	Category  string   `json:"category"`
	Desc      *string  `json:"desc"`
	URL       *string  `json:"url"`
	Icon      *string  `json:"icon"`
	OnDemand  bool     `json:"onDemand"`
	Port      *int     `json:"port"`
	Tags      []string `json:"tags"`
	Status    string   `json:"status"`
	ProbeType *string  `json:"probeType"`
	LatencyMs *int64   `json:"latencyMs"`
	Detail    *string  `json:"detail"`
	Error     *string  `json:"error"`
}

// DiscoveredEntry 是未登记发现条目。
type DiscoveredEntry struct {
	Port    int     `json:"port"`
	Process *string `json:"process"`
	URL     *string `json:"url"`
}

// CategoryAgg 是分类聚合项。
type CategoryAgg struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Total int    `json:"total"`
	Up    int    `json:"up"`
	Down  int    `json:"down"`
}

// collectPayload 是一次完整采集的返回载荷。
//
// 字段顺序即 JSON 键顺序，必须与旧实现 res.json 一致：旧实现在 collectOnce 结果上
// 展开 `{ ...payload, cached }`，故 cached 永远排在最后（discovered 之后）。cached 本身
// 是旧实现响应的一部分（命中 10s 缓存 true，否则 false），不能删。
type collectPayload struct {
	GeneratedAt   string            `json:"generatedAt"`
	TTLSeconds    int               `json:"ttlSeconds"`
	RegistryPath  string            `json:"registryPath"`
	RegistryMtime *float64          `json:"registryMtime"`
	Notice        *string           `json:"notice"`
	Warning       *string           `json:"warning"`
	Categories    []CategoryAgg     `json:"categories"`
	Apps          []AppView         `json:"apps"`
	Discovered    []DiscoveredEntry `json:"discovered"`
	Cached        bool              `json:"cached"`
}

/* ============ JS 语义小工具（登记表是外部输入，逐字段按旧实现归一化） ============ */

// jsString 复刻 JS 的 String(v)；JSON 里的 null 对应 "null"。
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(t)
	default:
		return fmt.Sprint(v)
	}
}

// jsNumberString 复刻 JS 数字转字符串：整数不带小数点。
func jsNumberString(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsTruthy 复刻 JS 的真值判定（仅覆盖 JSON 会出现的类型）。
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true
	}
}

// jsFalsy 是 jsTruthy 的反面，供 `x || y` 的取值选择使用。
func jsFalsy(v any) bool { return !jsTruthy(v) }

// jsNumber 复刻 JS 的 Number(v)（含 null→0、""→0、非法→NaN）。ok 表示结果有限。
func jsNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, true
	case float64:
		return t, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// truncate 复刻 `String(v ?? ”)` 再截断到 n 个字符（非 null 一律字符串化）。
func truncate(v any, n int) string {
	s := ""
	if v != nil {
		s = jsString(v)
	}
	return cutRunes(s, n)
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// strOrNil 复刻 `typeof v === 'string' && v ? v : null`。
func strOrNil(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

// ptrString 返回字符串指针，便于构造可空字段。
func ptrString(s string) *string { return &s }

// errOrNil 复刻 `pr.error || null`：空串与 nil 一律归为 nil。
func errOrNil(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// ptrI64 返回 int64 指针。
func ptrI64(v int64) *int64 { return &v }
