package files

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

// listEntry 是列目录返回的单个条目（JSON 键顺序与旧实现 res.json 一致）。
type listEntry struct {
	Name  string  `json:"name"`
	Type  string  `json:"type"`
	Size  int64   `json:"size"`
	Mtime float64 `json:"mtime"`
}

// filesListResponse 是列目录响应体；字段顺序 path→parent→entries 对齐旧实现，
// 不能用 map（encoding/json 会按键名字典序输出，得到 entries/parent/path 的错误顺序）。
type filesListResponse struct {
	Path    string      `json:"path"`
	Parent  *string     `json:"parent"`
	Entries []listEntry `json:"entries"`
}

// FilesList 处理 GET /api/admin/files：列目录，目录在前、同类按名称排序，跳过符号链接与受保护根项。
func (s *Service) FilesList(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.ResolveRel(r.URL.Query().Get("path"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	st, err := os.Lstat(dir)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "目录不存在")
		return
	}
	if !st.IsDir() {
		httpx.Error(w, http.StatusBadRequest, "不是目录")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "读取目录失败："+err.Error())
		return
	}
	out := make([]listEntry, 0, len(entries))
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := os.Lstat(full)
		if err != nil {
			continue
		}
		isDir := info.IsDir()
		// 跳过符号链接与特殊文件（防逃逸）
		if !isDir && !info.Mode().IsRegular() {
			continue
		}
		// 根目录保护名单项不展示
		if s.isHiddenRoot(full) {
			continue
		}
		size := info.Size()
		if isDir {
			size = 0
		}
		typ := "file"
		if isDir {
			typ = "dir"
		}
		// 复刻 Node stat.mtimeMs：整秒毫秒 + 纳秒折算，避免 UnixMilli 的截断误差。
		mt := info.ModTime()
		mtime := float64(mt.Unix())*1000 + float64(mt.Nanosecond())/1e6
		out = append(out, listEntry{Name: e.Name(), Type: typ, Size: size, Mtime: mtime})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "dir"
		}
		return lessByName(out[i].Name, out[j].Name)
	})
	rel := s.RelFromFull(dir)
	var parent *string
	if rel != "" {
		p := ""
		if i := strings.LastIndex(rel, "/"); i >= 0 {
			p = rel[:i]
		}
		parent = &p
	}
	httpx.WriteJSON(w, http.StatusOK, filesListResponse{Path: rel, Parent: parent, Entries: out})
}

// FilesDownload 处理 GET /api/admin/files/download：下载文件区内的单个普通文件。
func (s *Service) FilesDownload(w http.ResponseWriter, r *http.Request) {
	full, ok := s.ResolveRel(r.URL.Query().Get("path"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	if s.IsProtected(full) {
		httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许操作")
		return
	}
	st, err := os.Lstat(full)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	if !st.Mode().IsRegular() {
		httpx.Error(w, http.StatusBadRequest, "不是普通文件")
		return
	}
	s.ServeAttachment(w, r, full, filepath.Base(full))
}

// FilesMkdir 处理 POST /api/admin/files/mkdir：body {path,name}，建单层目录。
func (s *Service) FilesMkdir(w http.ResponseWriter, r *http.Request) {
	body := httpx.Body(r)
	dir, ok := s.ResolveRel(httpx.BodyString(body, "path"))
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	name := sanitizeSegment(httpx.BodyString(body, "name"))
	if name == "" {
		httpx.Error(w, http.StatusBadRequest, "名称非法")
		return
	}
	target := filepath.Join(dir, name)
	if !strings.HasPrefix(target, s.fileDir+string(os.PathSeparator)) {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	if s.IsProtected(target) {
		httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许操作")
		return
	}
	if pathExists(target) {
		httpx.Error(w, http.StatusConflict, "同名已存在")
		return
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		util.Audit(s.cfg.AuditLog, "mkdir", util.ClientIP(r), false, name+": "+err.Error())
		httpx.Error(w, http.StatusInternalServerError, "创建失败："+err.Error())
		return
	}
	rel := s.RelFromFull(target)
	util.Audit(s.cfg.AuditLog, "mkdir", util.ClientIP(r), true, rel)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel})
}

// FilesRename 处理 POST /api/admin/files/rename：body {path,name}，重命名文件或目录。
func (s *Service) FilesRename(w http.ResponseWriter, r *http.Request) {
	body := httpx.Body(r)
	full, ok := s.ResolveRel(httpx.BodyString(body, "path"))
	if !ok || full == s.fileDir {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	name := sanitizeSegment(httpx.BodyString(body, "name"))
	if name == "" {
		httpx.Error(w, http.StatusBadRequest, "名称非法")
		return
	}
	target := filepath.Join(filepath.Dir(full), name)
	if !strings.HasPrefix(target, s.fileDir+string(os.PathSeparator)) {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	// 源路径或目标路径命中根目录保护名单都拒绝（防止改名躲过保护 / 改名占位）
	if s.IsProtected(full) || s.IsProtected(target) {
		httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许操作")
		return
	}
	if !pathExists(full) {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	if target != full && pathExists(target) {
		httpx.Error(w, http.StatusConflict, "同名已存在")
		return
	}
	if err := os.Rename(full, target); err != nil {
		util.Audit(s.cfg.AuditLog, "rename", util.ClientIP(r), false, s.RelFromFull(full)+": "+err.Error())
		httpx.Error(w, http.StatusInternalServerError, "重命名失败："+err.Error())
		return
	}
	util.Audit(s.cfg.AuditLog, "rename", util.ClientIP(r), true, s.RelFromFull(full)+" → "+s.RelFromFull(target))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.RelFromFull(target)})
}

// FilesDelete 处理 DELETE /api/admin/files：删除文件或目录（目录递归）。
func (s *Service) FilesDelete(w http.ResponseWriter, r *http.Request) {
	full, ok := s.ResolveRel(r.URL.Query().Get("path"))
	if !ok || full == s.fileDir {
		httpx.Error(w, http.StatusBadRequest, "路径非法")
		return
	}
	if s.IsProtected(full) {
		httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许操作")
		return
	}
	st, err := os.Lstat(full)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	rel := s.RelFromFull(full)
	if st.IsDir() {
		err = os.RemoveAll(full)
	} else {
		err = os.Remove(full)
	}
	if err != nil {
		util.Audit(s.cfg.AuditLog, "delete", util.ClientIP(r), false, rel+": "+err.Error())
		httpx.Error(w, http.StatusInternalServerError, "删除失败："+err.Error())
		return
	}
	util.Audit(s.cfg.AuditLog, "delete", util.ClientIP(r), true, rel)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Download 处理 GET /api/admin/download：白名单根目录内的通用下载，支持 ~/ 展开。
func (s *Service) Download(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		httpx.Error(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	expanded := p
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, p[2:])
		}
	}
	resolved, err := filepath.Abs(expanded)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "路径不在允许范围内")
		return
	}
	allowed := false
	for _, base := range s.allowedBases() {
		if resolved == base || strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		httpx.Error(w, http.StatusBadRequest, "路径不在允许范围内")
		return
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		httpx.Error(w, http.StatusNotFound, "文件不存在")
		return
	}
	s.ServeAttachment(w, r, resolved, filepath.Base(resolved))
}

// allowedBases 返回通用下载白名单根目录（绝对路径），对应旧实现 DOWNLOAD_ALLOWED_BASES。
func (s *Service) allowedBases() []string {
	candidates := []string{s.uploadDir, "/root", "/tmp", "/home", "/var/www"}
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if strings.HasPrefix(abs, string(os.PathSeparator)) {
			out = append(out, abs)
		}
	}
	return out
}
