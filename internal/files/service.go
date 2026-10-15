// Package files 实现通用上传/下载与「文件区」目录浏览、上传、新建、重命名、删除。
//
// 行为逐条对齐旧实现 src/routes/files.ts：所有文件区路径严格限制在 FILE_DIR 内，
// 列表跳过符号链接与特殊文件，根目录保护名单（隐藏 + 禁写）与只读名单只作用于根这一层。
// ResolveRel / IsProtected / RelFromFull / FileDir 一并导出，供 shares 复用同一套判定。
package files

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"admin-server-go/internal/config"
)

// fileRootProtected 是根目录这一层的保护名单：既不展示、整棵子树也不允许写操作。
// 与旧实现 FILE_ROOT_PROTECTED 一字不差。
var fileRootProtected = map[string]bool{
	"swapfile":     true,
	"lost+found":   true,
	"cache":        true,
	"docker":       true,
	".trash-queue": true,
	"drop":         true,
}

// fileRootReadonly 是根目录这一层的收纳目录：正常展示、可浏览，但「根这一层的那一项」
// 不允许被删/改名/移动，子路径照常读写。与旧实现 FILE_ROOT_READONLY 一字不差。
var fileRootReadonly = map[string]bool{
	"toolchains":      true,
	"apps":            true,
	"build":           true,
	"www":             true,
	"files":           true,
	"backups":         true,
	"siyuan":          true,
	"vaultwarden":     true,
	"objbox":          true,
	"karakeep":        true,
	"davbox":          true,
	"memos":           true,
	"joplin":          true,
	"changedetection": true,
	"sillytavern":     true,
	"aionui":          true,
	"redroid":         true,
	"sftpgo":          true,
	"authgw":          true,
	"reminderweb":     true,
}

// Options 是构造 Service 的依赖。
type Options struct {
	Cfg *config.Config
	Log *slog.Logger
}

// Service 持有文件区根目录、上传目录与审计/日志依赖。
type Service struct {
	cfg       *config.Config
	log       *slog.Logger
	fileDir   string
	uploadDir string
}

// New 构造文件区服务。构造时按旧实现模块加载行为尝试创建 FILE_DIR 与 UPLOAD_DIR，
// 失败只记日志、不阻断（与旧实现一致）。
func New(opts Options) *Service {
	s := &Service{
		cfg:       opts.Cfg,
		log:       opts.Log,
		fileDir:   filepath.Clean(opts.Cfg.FileDir),
		uploadDir: filepath.Clean(opts.Cfg.UploadDir),
	}
	if err := os.MkdirAll(s.fileDir, 0o755); err != nil {
		s.warn("文件区目录创建失败", err)
	}
	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		s.warn("上传目录创建失败", err)
	}
	return s
}

// FileDir 返回文件区根目录（已 Clean 的绝对/配置路径）。
func (s *Service) FileDir() string { return s.fileDir }

// UploadDir 返回通用上传落点目录。
func (s *Service) UploadDir() string { return s.uploadDir }

// ResolveRel 把外部相对路径解析为 FILE_DIR 内的绝对路径；越界、非法转义或非法 UTF-8 返回 false。
//
// 与旧实现 resolveFileRel 一致：多余的前导斜杠被剥离后再 resolve+Clean，因此
// "/etc/passwd" 会被收拢到 FILE_DIR/etc/passwd；而 "../" 这类逃逸在边界检查处被拒。
func (s *Service) ResolveRel(rel string) (string, bool) {
	decoded, ok := decodeURIComponent(rel)
	if !ok {
		return "", false
	}
	full := filepath.Clean(filepath.Join(s.fileDir, strings.TrimLeft(decoded, "/")))
	if full != s.fileDir && !strings.HasPrefix(full, s.fileDir+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// IsProtected 判断已解析的绝对路径是否命中文件区根目录保护名单。
// 规则与旧实现 isProtectedPath 一致：取相对 FILE_DIR 的第一段，命中 PROTECTED 整棵子树都算，
// 命中 READONLY 只算根这一层；根目录自身与越界路径一律返回 false。
func (s *Service) IsProtected(full string) bool {
	if full == s.fileDir {
		return false
	}
	parts, ok := s.relParts(full)
	if !ok {
		return false
	}
	if fileRootProtected[parts[0]] {
		return true
	}
	return len(parts) == 1 && fileRootReadonly[parts[0]]
}

// isHiddenRoot 判断路径是否属于「连列表里都不展示」的那一类，只用于列目录过滤。
func (s *Service) isHiddenRoot(full string) bool {
	parts, ok := s.relParts(full)
	if !ok {
		return false
	}
	return fileRootProtected[parts[0]]
}

// RelFromFull 把绝对路径转换为相对 FILE_DIR、以 / 分隔的路径；根目录返回空串。
func (s *Service) RelFromFull(full string) string {
	rel, err := filepath.Rel(s.fileDir, full)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// relParts 把绝对路径拆成相对 FILE_DIR 的路径段；越界或等于根目录时返回 false。
func (s *Service) relParts(full string) ([]string, bool) {
	rel, err := filepath.Rel(s.fileDir, full)
	if err != nil || rel == "" || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, false
	}
	return strings.Split(rel, string(os.PathSeparator)), true
}

func (s *Service) warn(msg string, err error) {
	if s.log != nil {
		s.log.Warn(msg, "err", err)
	}
}

// decodeURIComponent 与 JS decodeURIComponent 等价：单次百分号解码，非法转义或解码结果
// 不是合法 UTF-8 时返回 false（旧实现此时返回 null）。
func decodeURIComponent(raw string) (string, bool) {
	out, err := url.PathUnescape(raw)
	if err != nil {
		return "", false
	}
	if !utf8.ValidString(out) {
		return "", false
	}
	return out, true
}
