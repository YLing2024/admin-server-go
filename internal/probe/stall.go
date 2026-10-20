package probe

import "math"

// 卡顿指数权重。CPU 争抢最直接决定「点了没反应」，给最高权重；IO 等待次之；
// 内存回收通常只在接近上限时才有感；运行队列是瞬时信号、抖动大，只用来加权修正，
// 不让它单独把分数推高。权重集中在这里，避免后人凭感觉改单点系数。
const (
	stallWeightCPU      = 0.40
	stallWeightIO       = 0.30
	stallWeightMemory   = 0.20
	stallWeightRunQueue = 0.10
)

// computeStallScore 把 PSI 三档 avg60 与运行队列压成 0–100 的卡顿指数。
// 分数只描述「现在卡不卡」，不替代原始指标；档位文案由后端给出，前端不重复计算。
func computeStallScore(psi map[string]any, runQueue, cores int) (float64, string) {
	cpu := math.Min(psiAvg60(psi, "cpu"), 100)
	io := math.Min(psiAvg60(psi, "io"), 100)
	mem := math.Min(psiAvg60(psi, "memory"), 100)

	queue := 0.0
	if cores > 0 {
		queue = math.Min(float64(runQueue)/float64(cores)*100, 100)
	}
	score := round1(stallWeightCPU*cpu +
		stallWeightIO*io +
		stallWeightMemory*mem +
		stallWeightRunQueue*queue)

	switch {
	case score < 20:
		return score, "顺畅"
	case score <= 50:
		return score, "有压力"
	default:
		return score, "明显卡顿"
	}
}

// psiAvg60 从 readPressure 的响应结构里取某一类的 some.avg60；
// 文件不存在、容器禁用 PSI 或结构异常时按 0 处理（不让缺数据变成负分或 NaN）。
func psiAvg60(psi map[string]any, kind string) float64 {
	entry, ok := psi[kind].(map[string]any)
	if !ok {
		return 0
	}
	some, ok := entry["some"].(map[string]any)
	if !ok {
		return 0
	}
	v, ok := some["avg60"].(float64)
	if !ok || math.IsNaN(v) || v < 0 {
		return 0
	}
	return v
}
