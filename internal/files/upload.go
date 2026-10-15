package files

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/util"
)

const (
	// FileMaxBytes 是文件区单文件上限（500MB），与通用上传的 100MB 相互独立。
	FileMaxBytes = 500 * 1024 * 1024
	// uploadMaxBytes 是通用上传 /api/admin/upload 的单文件上限（100MB）。
	uploadMaxBytes = 100 * 1024 * 1024
	// genericSizeError 是旧实现全局错误处理器对所有 multer 错误使用的固定文案。
	genericSizeError = "文件过大（上限 100MB）"
)

// 上传流程内部错误哨兵：用于把 multer 的语义映射回状态码与文案。
var (
	errNoFile          = errors.New("no file")
	errTooLarge        = errors.New("file too large")
	errUnexpectedField = errors.New("Unexpected field")
)

// uploadedFile 描述一次上传最终落盘的结果。
type uploadedFile struct {
	name string
	full string
	size int64
}

// Upload 处理 POST /api/admin/upload：multipart 字段 file（100MB），按时间戳命名落 uploads/。
func (s *Service) Upload(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	up, err := s.receiveFile(r, uploadMaxBytes, func(originalName string) (string, string, error) {
		name := fmt.Sprintf("%d-%d%s", config.NowMillis(), randInt1e9(), nodeExtname(originalName))
		return s.uploadDir, name, nil
	})
	switch {
	case errors.Is(err, errNoFile):
		util.Audit(s.cfg.AuditLog, "upload", ip, false, "未收到文件")
		httpx.Error(w, http.StatusBadRequest, "未收到文件（字段名应为 file）")
	case errors.Is(err, errTooLarge):
		util.Audit(s.cfg.AuditLog, "upload", ip, false, genericSizeError)
		httpx.Error(w, http.StatusRequestEntityTooLarge, genericSizeError)
	case errors.Is(err, errUnexpectedField):
		util.Audit(s.cfg.AuditLog, "upload", ip, false, genericSizeError)
		httpx.Error(w, http.StatusBadRequest, genericSizeError)
	case err != nil:
		util.Audit(s.cfg.AuditLog, "upload", ip, false, "服务器内部错误")
		httpx.Error(w, http.StatusInternalServerError, "服务器内部错误")
	default:
		util.Audit(s.cfg.AuditLog, "upload", ip, true, up.full)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"path": up.full})
	}
}

// FilesUpload 处理 POST /api/admin/files/upload：query path 指定目标目录（500MB），保留原始文件名。
func (s *Service) FilesUpload(w http.ResponseWriter, r *http.Request) {
	ip := util.ClientIP(r)
	queryPath := r.URL.Query().Get("path")
	up, err := s.receiveFile(r, FileMaxBytes, func(originalName string) (string, string, error) {
		dir, ok := s.ResolveRel(queryPath)
		if !ok {
			return "", "", errors.New("目标路径非法")
		}
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return "", "", fmt.Errorf("创建目标目录失败：%v", mkErr)
		}
		return dir, uniqueFileName(dir, latin1Name(originalName)), nil
	})
	tooLargeMsg := fmt.Sprintf("文件超过 %dMB 上限", FileMaxBytes/1024/1024)
	switch {
	case errors.Is(err, errNoFile):
		util.Audit(s.cfg.AuditLog, "upload", ip, false, "未收到文件")
		httpx.Error(w, http.StatusBadRequest, "未收到文件（字段名应为 file）")
	case errors.Is(err, errTooLarge):
		util.Audit(s.cfg.AuditLog, "upload", ip, false, tooLargeMsg)
		httpx.Error(w, http.StatusRequestEntityTooLarge, tooLargeMsg)
	case err != nil:
		msg := "上传失败：" + err.Error()
		util.Audit(s.cfg.AuditLog, "upload", ip, false, msg)
		httpx.Error(w, http.StatusBadRequest, msg)
	default:
		if s.IsProtected(up.full) {
			_ = os.Remove(up.full)
			util.Audit(s.cfg.AuditLog, "upload", ip, false, up.name+": 受保护路径")
			httpx.Error(w, http.StatusForbidden, "该文件受保护，不允许操作")
			return
		}
		util.Audit(s.cfg.AuditLog, "upload", ip, true, fmt.Sprintf("%s (%dB)", up.name, up.size))
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": up.name, "size": up.size, "path": up.full})
	}
}

// receiveFile 解析单个字段名为 file 的 multipart 请求并流式落盘。
// target 在读到文件段后、写入前被调用，返回目标目录与文件名（对齐 multer 的
// destination/filename 两个回调）。语义对齐 multer.single('file') + limits.fileSize。
func (s *Service) receiveFile(r *http.Request, maxBytes int64, target func(originalName string) (string, string, error)) (uploadedFile, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return uploadedFile{}, errNoFile
	}
	var result uploadedFile
	found := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// 已落盘的文件在后续解析错误时一并清理（对齐 multer 的 removeUploadedFiles）。
			if found {
				_ = os.Remove(result.full)
			}
			return uploadedFile{}, err
		}
		if part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if found || part.FormName() != "file" {
			_ = part.Close()
			if found {
				_ = os.Remove(result.full)
			}
			return uploadedFile{}, errUnexpectedField
		}
		dir, name, terr := target(part.FileName())
		if terr != nil {
			_ = part.Close()
			return uploadedFile{}, terr
		}
		full := filepath.Join(dir, name)
		size, perr := persistPart(part, full, maxBytes)
		_ = part.Close()
		if perr != nil {
			return uploadedFile{}, perr
		}
		result = uploadedFile{name: name, full: full, size: size}
		found = true
	}
	if !found {
		return uploadedFile{}, errNoFile
	}
	return result, nil
}

// persistPart 把文件段写入磁盘；超过上限时删除已写入部分并返回 errTooLarge。
func persistPart(part *multipart.Part, full string, maxBytes int64) (int64, error) {
	f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(part, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(full)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(full)
		return n, closeErr
	}
	if n > maxBytes {
		_ = os.Remove(full)
		return n, errTooLarge
	}
	return n, nil
}

// randInt1e9 生成 0..1e9 的随机整数，对应旧实现 Math.round(Math.random()*1e9)。
func randInt1e9() int64 {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000000))
	if err != nil {
		return config.NowMillis() % 1000000000
	}
	return n.Int64()
}
