package files

import (
	"sort"
	"strings"
	"testing"
)

// TestLessByNameNodeAcceptance 用旧实现（Node 24 `localeCompare('zh-Hans-CN')`）实测的
// 一组中英混排名做验收：数字/点开头最前，汉字按拼音且整体排在拉丁之前。
func TestLessByNameNodeAcceptance(t *testing.T) {
	names := []string{".hidden", "啊.txt", "带空格 的.txt", "中文名.md", "a.txt", "blob.bin", "z.txt"}
	// Node 真值：['.hidden','啊.txt','带空格 的.txt','中文名.md','a.txt','blob.bin','z.txt']
	want := []string{".hidden", "啊.txt", "带空格 的.txt", "中文名.md", "a.txt", "blob.bin", "z.txt"}

	got := append([]string(nil), names...)
	sort.SliceStable(got, func(i, j int) bool { return lessByName(got[i], got[j]) })
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("排序 = %v, want %v", got, want)
	}
}

// TestLessByNameHanBeforeLatin 固定「汉字块整体排在拉丁之前」这一关键约束，
// 这正是 x/text 与旧实现 ICU 相反、需要 remap 的地方。
func TestLessByNameHanBeforeLatin(t *testing.T) {
	han := []string{"啊", "带", "中"}
	// 取汉字与拉丁/数字混合，逐一验证汉字 < 拉丁。
	cases := []struct {
		a, b string
		want bool // a 是否应排在 b 前
	}{
		{"啊", "a", true},
		{"中", "z", true},
		{"带", "blob", true},
		{".hidden", "啊.txt", true},
		{"啊.txt", "带空格 的.txt", true}, // 啊(a) 在 带(d) 之前
		{"带.txt", "中.txt", true},     // 带(d) 在 中(z) 之前
		{"中.txt", "a.txt", true},     // 汉字在拉丁之前
		{"a.txt", "z.txt", true},
	}
	for _, c := range cases {
		if got := lessByName(c.a, c.b); got != c.want {
			t.Errorf("lessByName(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	// 同组内拼音序：啊 < 带 < 中。
	sorted := append([]string(nil), han...)
	sort.SliceStable(sorted, func(i, j int) bool { return lessByName(sorted[i], sorted[j]) })
	if strings.Join(sorted, "") != "啊带中" {
		t.Fatalf("拼音序 = %v, want [啊 带 中]", sorted)
	}
}

// TestLessByNameStableOnEqual 主权重相同（如大小写差异）时比较器应当给出确定顺序且不自相矛盾。
func TestLessByNameStableOnEqual(t *testing.T) {
	a, b := "A.txt", "a.txt"
	ab, ba := lessByName(a, b), lessByName(b, a)
	if ab == ba {
		t.Fatalf("lessByName(%q,%q)=%v 与反向 %v 相同，比较器非严格弱序", a, b, ab, ba)
	}
}
