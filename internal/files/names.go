package files

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"admin-server-go/internal/config"
)

// decodeOriginalName 复刻旧实现：把调用方给的字符串按「每个码元的低 8 位」当成 Latin-1
// 字节，再尝试按 UTF-8 解码；解码结果不含替换字符时采用它，否则回退原值。
//
// 注意：multipart 文件名需先用 latin1Name 还原成 busboy 的 Latin-1 码元串再传入，
// 这样两边对「原始字节」的解释保持一致。
func decodeOriginalName(raw string) string {
	buf := make([]byte, 0, len(raw))
	for _, r := range raw {
		buf = append(buf, byte(r))
	}
	if utf8.Valid(buf) && !strings.Contains(string(buf), "\uFFFD") {
		return string(buf)
	}
	return raw
}

// latin1Name 把 Go 字符串按字节映射为「Latin-1 码元」串，等价于 busboy 对 Content-Disposition
// 的 latin1 解码结果，供 decodeOriginalName 还原出真正的 UTF-8 文件名。
func latin1Name(name string) string {
	runes := make([]rune, 0, len(name))
	for _, b := range []byte(name) {
		runes = append(runes, rune(b))
	}
	return string(runes)
}

// posixBasename 复刻 Node path.posix.basename：去掉尾部斜杠后取最后一段，根/空返回空串。
func posixBasename(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// nodeExtname 复刻 Node path.extname 的算法（含前导点、连续点的特殊规则）。
func nodeExtname(name string) string {
	startDot := -1
	startPart := 0
	end := -1
	matchedSlash := true
	preDotState := 0
	for i := len(name) - 1; i >= 0; i-- {
		c := name[i]
		if c == '/' {
			if !matchedSlash {
				startPart = i + 1
				break
			}
			continue
		}
		if end == -1 {
			matchedSlash = false
			end = i + 1
		}
		if c == '.' {
			if startDot == -1 {
				startDot = i
			} else if preDotState != 1 {
				preDotState = 1
			}
		} else if startDot != -1 {
			preDotState = -1
		}
	}
	if startDot == -1 || end == -1 || preDotState == 0 ||
		(preDotState == 1 && startDot == end-1 && startDot == startPart+1) {
		return ""
	}
	return name[startDot:end]
}

// sanitizeSegment 复刻旧实现 sanitizeSegment：取 basename、去掉控制字符与斜杠、trim，
// 并拒绝空 / "." / ".."。
func sanitizeSegment(raw string) string {
	base := posixBasename(decodeOriginalName(raw))
	var b strings.Builder
	for _, r := range base {
		if r <= 0x1f || r == 0x7f || r == '\\' || r == '/' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}

// uniqueFileName 复刻旧实现 uniqueFileName：重名不覆盖，notes.7z → notes-2.7z → notes-3.7z；
// 无有效名时用 unnamed，耗尽 9999 次后拼接时间戳兜底。
func uniqueFileName(dir, raw string) string {
	name := sanitizeSegment(raw)
	if name == "" {
		name = "unnamed"
	}
	if !pathExists(filepath.Join(dir, name)) {
		return name
	}
	ext := nodeExtname(name)
	stem := name[:len(name)-len(ext)]
	if stem == "" {
		stem = "file"
	}
	for i := 2; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if !pathExists(filepath.Join(dir, candidate)) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", stem, config.NowMillis(), ext)
}

// pathExists 与旧实现 fs.existsSync 一致：跟随符号链接判断是否存在。
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
