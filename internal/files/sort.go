package files

import (
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// comparator 复刻 Node `a.localeCompare(b, 'zh-Hans-CN')` 的文件名排序。
//
// 旧实现用 ICU 的 zh 排序：数字/标点/空格等「通用字符」最前，其次是汉字（按拼音），
// 再次是拉丁等其它字母文字。x/text 的排序表冻结在 CLDR 23，汉字块位置与当前 ICU 相反
// （它把汉字排在拉丁之后），且对少数生僻字缺少拼音（见 COMPAT.md）。
// 因此这里自己按「脚本分组 → 组内 x/text 主权重」比较，把汉字块摆回拉丁之前，
// 组内（拼音、大小写、音调）仍交给 x/text，避免重造整张排序表。
var comparator = collate.New(language.Chinese)

// 脚本分组：0=数字/空格/标点/符号等通用字符，1=汉字，2=其余（拉丁等）。
// 顺序即旧实现 ICU zh 的脚本块顺序。
const (
	groupCommon byte = iota
	groupHan
	groupOther
)

// collUnit 是一个排序原子：脚本分组 + x/text 主权重。
type collUnit struct {
	group  byte
	weight uint32
}

// groupOf 把码点归入脚本分组。
func groupOf(r rune) byte {
	switch {
	case unicode.Is(unicode.Han, r):
		return groupHan
	case unicode.IsNumber(r) || unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
		return groupCommon
	default:
		return groupOther
	}
}

// primaryWeights 解析 x/text 排序键的第一级（主权重）。键的格式是若干权重字节：
// 首字节最高位为 1 表示三字节权重，否则两字节；权重序列以 0x00 分隔各级。
func primaryWeights(key []byte) []uint32 {
	out := make([]uint32, 0, len(key)/2)
	for i := 0; i+1 < len(key); {
		b := key[i]
		if b == 0 {
			break
		}
		if b&0x80 != 0 {
			if i+2 >= len(key) {
				break
			}
			out = append(out, uint32(b&0x7f)<<16|uint32(key[i+1])<<8|uint32(key[i+2]))
			i += 3
		} else {
			out = append(out, uint32(b)<<8|uint32(key[i+1]))
			i += 2
		}
	}
	return out
}

// sortUnits 把名字拆成排序原子序列。组合记号不单独参与主序；无主权重的码点跳过。
func sortUnits(s string) []collUnit {
	var buf collate.Buffer
	out := make([]collUnit, 0, len(s))
	for _, r := range s {
		if unicode.Is(unicode.M, r) {
			continue
		}
		weights := primaryWeights(comparator.KeyFromString(&buf, string(r)))
		if len(weights) == 0 {
			continue
		}
		g := groupOf(r)
		for _, w := range weights {
			out = append(out, collUnit{group: g, weight: w})
		}
	}
	return out
}

// lessByName 是列目录用的名称比较器（目录/文件分组由调用方处理）。
func lessByName(a, b string) bool {
	ua, ub := sortUnits(a), sortUnits(b)
	n := len(ua)
	if len(ub) < n {
		n = len(ub)
	}
	for i := 0; i < n; i++ {
		if ua[i].group != ub[i].group {
			return ua[i].group < ub[i].group
		}
		if ua[i].weight != ub[i].weight {
			return ua[i].weight < ub[i].weight
		}
	}
	if len(ua) != len(ub) {
		return len(ua) < len(ub)
	}
	// 主权重完全一致（例如大小写/音调差异）时，落到 x/text 的次级/三级比较。
	return comparator.CompareString(a, b) < 0
}
