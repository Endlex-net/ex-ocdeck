// Package uploads 实现受管上传存储（terminal-file-paste-drop 2.2，design D4）：
// 布局 <root>/<taskID>/<32hex><可选 .ext>，task 目录 0700、文件 0600；辅助文件为
// <name>.partial（上传中）与 <name>.meta.json（sidecar，六键 JSON 唯一持久真值），
// sidecar 经 <name>.meta.json.tmp 原子 rename 落盘。
//
// 洋葱边界：本包实现 internal/application/uploads 的 StorePort（允许 import
// application）；命名/提交顺序/扫描分类语义以该包端口注释与 design D4 为准。
package uploads

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	appuploads "ocdeck/internal/application/uploads"
)

// 文件布局后缀（本包私有：应用层只见语义化端口方法）。
const (
	partialSuffix = ".partial"
	metaSuffix    = ".meta.json"
	// metaTmpSuffix sidecar 中间文件后缀（提交与刷新共用，遗留按孤儿清理）。
	metaTmpSuffix = ".meta.json.tmp"
)

var (
	// extRule 扩展名规则：原始名小写化后匹配 ^\.[a-z0-9]{1,10}$ 才保留为落盘扩展名。
	extRule = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)
	// managedNameRule 受管落盘名规则（配对不变量之一）。uploadID 32hex 判定
	// 复用 appuploads.IsUploadID（单一真相源）。
	managedNameRule = regexp.MustCompile(`^[0-9a-f]{32}(\.[a-z0-9]{1,10})?$`)
)

// Store 受管上传存储。
type Store struct {
	root string
}

// NewStore 构造存储（root 来自 config.UploadDir，加载期已绝对化并创建）。
func NewStore(root string) *Store {
	return &Store{root: root}
}

// Root 返回存储根目录（测试与组合根诊断用）。
func (s *Store) Root() string { return s.root }

// NewUploadID 生成 32hex 上传 ID（crypto/rand 16B；Go 1.24 Read 永不返回 error，
// 错误分支为防御性保留）。
func (s *Store) NewUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("uploads: generate upload id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ManagedName 计算落盘名：uploadID + 合法扩展名规则；客户端原始名不进入落盘路径。
func (s *Store) ManagedName(uploadID, originalName string) string {
	ext := strings.ToLower(filepath.Ext(originalName))
	if extRule.MatchString(ext) {
		return uploadID + ext
	}
	return uploadID
}

// taskDir 任务存储目录路径。
func (s *Store) taskDir(taskID string) string {
	return filepath.Join(s.root, taskID)
}

// WritePartial 流式写 name 的 .partial：task 目录 0700、文件 0600；每个非零写入块
// 调用 onProgress；失败尽力清理残留。
func (s *Store) WritePartial(ctx context.Context, taskID, name string, r io.Reader, onProgress func(n int64)) (int64, error) {
	dir := s.taskDir(taskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("uploads: create task dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+partialSuffix)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("uploads: open partial %s: %w", path, err)
	}
	n, werr := s.copyWithProgress(ctx, f, r, onProgress)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			log.Printf("uploads: cleanup failed partial %s: %v (treated as orphan)", path, rerr)
		}
		return n, werr
	}
	return n, nil
}

// copyWithProgress 流式复制：每个非零写入块后调用 onProgress；感知 ctx 取消。
func (s *Store) copyWithProgress(ctx context.Context, w io.Writer, r io.Reader, onProgress func(n int64)) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			total += int64(wn)
			if werr != nil {
				return total, werr
			}
			if onProgress != nil {
				onProgress(int64(wn))
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// CommitUpload 提交上传（提交顺序钉死，design D4）：
// .partial → 原子 rename 最终名 → sidecar .tmp → 原子 rename .meta.json。
// sidecar 失败：尽力撤销已 rename 文件（撤销失败记日志、按孤儿处理）并返回错误。
func (s *Store) CommitUpload(taskID string, meta appuploads.Meta) error {
	dir := s.taskDir(taskID)
	finalPath := filepath.Join(dir, meta.Filename)
	partialPath := finalPath + partialSuffix
	if err := os.Rename(partialPath, finalPath); err != nil {
		return fmt.Errorf("uploads: commit rename %s: %w", partialPath, err)
	}
	tmpPath := finalPath + metaTmpSuffix
	if err := writeSidecarTmp(tmpPath, meta); err != nil {
		os.Remove(tmpPath)
		s.undoCommittedFile(finalPath)
		return fmt.Errorf("uploads: write sidecar %s: %w", tmpPath, err)
	}
	metaPath := finalPath + metaSuffix
	if err := os.Rename(tmpPath, metaPath); err != nil {
		os.Remove(tmpPath)
		s.undoCommittedFile(finalPath)
		return fmt.Errorf("uploads: commit sidecar rename %s: %w", tmpPath, err)
	}
	return nil
}

// undoCommittedFile sidecar 失败时撤销已 rename 的上传文件（尽力；失败按孤儿处理）。
func (s *Store) undoCommittedFile(finalPath string) {
	if err := os.Remove(finalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("uploads: undo committed file %s: %v (treated as orphan)", finalPath, err)
	}
}

// StatUpload 报告最终名文件是否存在（NotExist 归一化为 (false, nil)）。
func (s *Store) StatUpload(taskID, filename string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.taskDir(taskID), filename))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("uploads: stat %s/%s: %w", taskID, filename, err)
}

// RefreshLastDelivered 以 meta + at 重写 sidecar（.tmp → 原子 rename；失败保留旧文件）。
func (s *Store) RefreshLastDelivered(taskID string, meta appuploads.Meta, at time.Time) error {
	metaPath := filepath.Join(s.taskDir(taskID), meta.Filename+metaSuffix)
	tmpPath := metaPath + ".tmp"
	updated := meta
	updated.LastDeliveredAt = &at
	if err := writeSidecarTmp(tmpPath, updated); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("uploads: refresh sidecar %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("uploads: refresh rename %s: %w", tmpPath, err)
	}
	return nil
}

// RemoveUpload 删除文件与其 sidecar（均容忍 NotExist；返回首个非 NotExist 错误）。
func (s *Store) RemoveUpload(taskID, filename string) error {
	dir := s.taskDir(taskID)
	var firstErr error
	if err := os.Remove(filepath.Join(dir, filename)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		firstErr = fmt.Errorf("uploads: remove %s/%s: %w", taskID, filename, err)
	}
	if err := os.Remove(filepath.Join(dir, filename+metaSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
		firstErr = fmt.Errorf("uploads: remove sidecar %s/%s: %w", taskID, filename+metaSuffix, err)
	}
	return firstErr
}

// RemovePartial 删除 name 的 .partial（容忍 NotExist）。
func (s *Store) RemovePartial(taskID, name string) error {
	if err := os.Remove(filepath.Join(s.taskDir(taskID), name+partialSuffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("uploads: remove partial %s/%s: %w", taskID, name, err)
	}
	return nil
}

// RemovePath 删除 task 目录下精确相对路径（容忍 NotExist；孤儿清理通用）。
func (s *Store) RemovePath(taskID, filename string) error {
	if err := os.Remove(filepath.Join(s.taskDir(taskID), filename)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("uploads: remove %s/%s: %w", taskID, filename, err)
	}
	return nil
}

// RemoveTaskDir 整目录回收（目录不存在视为成功）。
func (s *Store) RemoveTaskDir(taskID string) error {
	if err := os.RemoveAll(s.taskDir(taskID)); err != nil {
		return fmt.Errorf("uploads: remove task dir %s: %w", taskID, err)
	}
	return nil
}
