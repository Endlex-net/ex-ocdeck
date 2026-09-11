package uploads

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	appuploads "ocdeck/internal/application/uploads"
)

// Scan 扫描根目录并按 design D4 分类表分类（启动恢复与周期清理的唯一输入）：
//   - 有效配对（sidecar 全部不变量成立 + 同名配对文件存在）→ EntryValidUpload；
//   - 无 sidecar 的文件 → EntryOrphanFile；单边 sidecar → EntryOrphanSidecar；
//   - 损坏 sidecar 及其对应文件 → EntryOrphanCorrupt（「对应文件」固定指扫描位置
//     同名配对文件，不按损坏字段找其他路径）；
//   - 遗留 .partial → EntryStalePartial（Filename 为去后缀受管名）；遗留 .tmp → EntryOrphanTmp；
//   - sidecar 权限/I/O 读取失败 → EntryReadFailed（不归孤儿、不删、下轮重试）。
//
// 根目录不存在视为空；任务目录读取失败记日志本轮跳过。ModTime 取主文件
// （孤儿清理时间计算依据；文件 mtime 不重建过期基准）。
func (s *Store) Scan() ([]appuploads.Entry, error) {
	rootEntries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []appuploads.Entry
	for _, de := range rootEntries {
		if !de.IsDir() {
			continue // 根下非目录条目跳过
		}
		out = append(out, s.scanTaskDir(de.Name())...)
	}
	return out, nil
}

// scanTaskDir 分类单个任务目录。
func (s *Store) scanTaskDir(taskID string) []appuploads.Entry {
	dir := s.taskDir(taskID)
	des, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		log.Printf("uploads: scan task dir %s: %v (skipped this round)", dir, err)
		return nil
	}
	claimed := make(map[string]bool) // 已被 sidecar 认领的配对文件名
	var out []appuploads.Entry
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		switch {
		case strings.HasSuffix(name, metaTmpSuffix):
			out = append(out, entryWithModTime(de, appuploads.Entry{
				Kind: appuploads.EntryOrphanTmp, TaskID: taskID, Filename: name,
			}))
		case strings.HasSuffix(name, partialSuffix):
			out = append(out, entryWithModTime(de, appuploads.Entry{
				Kind:     appuploads.EntryStalePartial,
				TaskID:   taskID,
				Filename: strings.TrimSuffix(name, partialSuffix),
			}))
		case strings.HasSuffix(name, metaSuffix):
			pairedName := strings.TrimSuffix(name, metaSuffix)
			info, infoErr := de.Info()
			if infoErr != nil {
				// 读取失败：sidecar 及配对文件都不删（claim 配对路径防第二遍误归
				// OrphanFile 超龄删除），下轮重试。
				claimed[pairedName] = true
				out = append(out, appuploads.Entry{Kind: appuploads.EntryReadFailed, TaskID: taskID, Filename: name})
				continue
			}
			meta, rerr := s.readSidecar(filepath.Join(dir, name))
			switch {
			case errors.Is(rerr, errSidecarReadFailed):
				// 同上：读取失败不归孤儿、不删。
				claimed[pairedName] = true
				out = append(out, appuploads.Entry{Kind: appuploads.EntryReadFailed, TaskID: taskID, Filename: name, ModTime: info.ModTime()})
			case rerr != nil || !sidecarValid(meta, taskID, pairedName):
				out = append(out, appuploads.Entry{Kind: appuploads.EntryOrphanCorrupt, TaskID: taskID, Filename: pairedName, ModTime: info.ModTime()})
				claimed[pairedName] = true
			default:
				if _, statErr := os.Stat(filepath.Join(dir, pairedName)); statErr != nil {
					if errors.Is(statErr, fs.ErrNotExist) {
						// 配对文件确认不存在：单边 sidecar 孤儿。
						out = append(out, appuploads.Entry{Kind: appuploads.EntryOrphanSidecar, TaskID: taskID, Filename: name, ModTime: info.ModTime()})
					} else {
						// 权限/I/O 等读取失败：不归孤儿、不删、下轮重试；claim 配对文件
						// 防止第二遍误归 OrphanFile。
						claimed[pairedName] = true
						out = append(out, appuploads.Entry{Kind: appuploads.EntryReadFailed, TaskID: taskID, Filename: name, ModTime: info.ModTime()})
					}
					continue
				}
				out = append(out, appuploads.Entry{Kind: appuploads.EntryValidUpload, TaskID: taskID, Filename: pairedName, ModTime: info.ModTime(), Meta: &meta})
				claimed[pairedName] = true
			}
		}
	}
	// 第二遍：未被认领且非辅助后缀的文件 = 无 sidecar 文件（孤儿）。
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if claimed[name] ||
			strings.HasSuffix(name, metaTmpSuffix) ||
			strings.HasSuffix(name, partialSuffix) ||
			strings.HasSuffix(name, metaSuffix) {
			continue
		}
		out = append(out, entryWithModTime(de, appuploads.Entry{
			Kind: appuploads.EntryOrphanFile, TaskID: taskID, Filename: name,
		}))
	}
	return out
}

// entryWithModTime 附加条目主文件 modtime。Info 失败（I/O 抖动/悬空链接）时
// ModTime 保持零值：清理侧把零值视为「时间未知」跳过删除，下轮重试。
func entryWithModTime(de fs.DirEntry, e appuploads.Entry) appuploads.Entry {
	if info, err := de.Info(); err == nil {
		e.ModTime = info.ModTime()
	}
	return e
}
