package probe

import (
	"os"
	"regexp"
	"strings"
)

// pressureLineRe 匹配 PSI 文件里的 `some/full avg10=.. avg60=.. avg300=.. total=..` 行。
var pressureLineRe = regexp.MustCompile(`^(some|full)\s+avg10=([\d.]+)\s+avg60=([\d.]+)\s+avg300=([\d.]+)\s+total=(\d+)`)

// parsePressureFile 解析单个 /proc/pressure/<type>；文件不存在或解析失败（老内核/容器无 PSI）返回 nil。
func parsePressureFile(kind string) map[string]any {
	data, err := os.ReadFile("/proc/pressure/" + kind)
	if err != nil {
		return nil
	}
	res := map[string]map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		m := pressureLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		res[m[1]] = map[string]any{
			"avg10":  atof(m[2]),
			"avg60":  atof(m[3]),
			"avg300": atof(m[4]),
			"total":  atoi(m[5]),
		}
	}
	if res["some"] == nil || res["full"] == nil {
		return nil
	}
	return map[string]any{"some": res["some"], "full": res["full"]}
}

// readPressure 返回内存/CPU/IO 的 PSI；各自 { some, full } 或 nil。
func readPressure() map[string]any {
	return map[string]any{
		"memory": parsePressureFile("memory"),
		"cpu":    parsePressureFile("cpu"),
		"io":     parsePressureFile("io"),
	}
}

// emptyPressure 返回与 readPressure 同形状的空值，供采样器尚未产出第一帧时使用。
// 响应字段必须稳定存在，但请求路径不允许为了补空值去读 /proc。
func emptyPressure() map[string]any {
	return map[string]any{"memory": nil, "cpu": nil, "io": nil}
}

// PsiHist 是 PSI 历史取值：memory/cpu/io 的 some avg10（原生已是滑动均值）。
type PsiHist struct {
	MemAvg10 *float64
	CPUAvg10 *float64
	IOAvg10  *float64
}

// PsiHistPoint 取三类 PSI 的 some avg10，供历史采样使用。
func PsiHistPoint() PsiHist {
	some := func(kind string) *float64 {
		p := parsePressureFile(kind)
		if p == nil {
			return nil
		}
		entry, ok := p["some"].(map[string]any)
		if !ok {
			return nil
		}
		v, ok := entry["avg10"].(float64)
		if !ok {
			return nil
		}
		out := v
		return &out
	}
	return PsiHist{MemAvg10: some("memory"), CPUAvg10: some("cpu"), IOAvg10: some("io")}
}
