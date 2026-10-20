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

// 档位按严重程度排序，用于在「分数档位」之上叠加下限规则。
var stallLevelRank = map[string]int{
	"顺畅":   0,
	"有压力":  1,
	"明显卡顿": 2,
}

// computeStallScore 把 PSI 三档 avg60 与运行队列压成 0–100 的卡顿指数。
// 分数只描述「现在卡不卡」，不替代原始指标；档位文案由后端给出，前端不重复计算。
//
// 分数回答「等了多久」，档位回答「体感如何」：8 核跑满时排队已经发生，但 PSI 仍可能很低，
// 此时分数保持不动、只把档位抬到「有压力」，避免读者看到「满载却顺畅」而困惑。
// usage15s 只参与档位下限判定，不进入分数。
func computeStallScore(psi map[string]any, runQueue, cores int, usage15s float64) (float64, string) {
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

	level := "顺畅"
	switch {
	case score > 50:
		level = "明显卡顿"
	case score >= 20:
		level = "有压力"
	}
	if floor := stallLevelFloor(runQueue, cores, usage15s); stallLevelRank[floor] > stallLevelRank[level] {
		level = floor
	}
	return score, level
}

// stallLevelFloor 给出档位下限：排队超过核数的 1.5 倍，或 CPU 持续压满且已排队，
// 都说明资源竞争已经发生，档位至少应为「有压力」。
func stallLevelFloor(runQueue, cores int, usage15s float64) string {
	if cores > 0 && float64(runQueue) > float64(cores)*1.5 {
		return "有压力"
	}
	if usage15s >= 95 && runQueue > cores {
		return "有压力"
	}
	return "顺畅"
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
