package git

import (
	"context"
	"sort"
	"strings"
)

// MergeBase 计算两个 OID 的最近共同祖先（git merge-base <baseOID> <headOID>，
// git-page-enhancements D2：分支对比 three-dot 语义的 merge-base 一环）。
//
// exit 0 → 共同祖先 OID；exit 1（经 gitExitCode 判定）→ ErrNoMergeBase（无共同祖先）；
// 其他非零 exit（如 OID 不存在的 exit 128）透传原错误（StderrOf 可提取 stderr）。
// 仅以 exit code 判定，MUST NOT 匹配 stderr 文案。
//
// 只读操作，不进 repo 写锁。argv 数组、context 取消、有界读取复用 run()（exec.go）。
func MergeBase(ctx context.Context, dir, baseOID, headOID string) (string, error) {
	out, _, err := run(ctx, dir, "merge-base", baseOID, headOID)
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return "", ErrNoMergeBase
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ResolveHeadOID 解析当前 HEAD 的 OID（git rev-parse --verify --quiet --end-of-options HEAD，
// git-page-enhancements D2）。--end-of-options 与 ResolveBaseRef/ResolveRefOID 防护一致；
// --quiet 使失败时无输出， unborn 判定仅依赖 exit code。
//
// exit 0 → HEAD OID；exit 1（经 gitExitCode 判定）→ ErrUnbornHead（仓库有效但 HEAD 无任何
// 提交，/tmp 实测）；exit 128 等致命失败（仓库损坏等）透传原错误（StderrOf 可提取 stderr），
// MUST NOT 匹配 stderr 文案。
//
// 只读操作，不进 repo 写锁。argv 数组、context 取消、有界读取复用 run()（exec.go）。
func ResolveHeadOID(ctx context.Context, dir string) (string, error) {
	out, _, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "HEAD")
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return "", ErrUnbornHead
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// BranchDiffFiles 返回 merge-base 到 headOID 两棵 tree 间的变更文件列表
// （git diff --numstat -z <mbOID> <headOID>，git-page-enhancements D3）。
// 命令只读两棵 tree，天然不含工作区/index 变更；mbOID == headOID 时返回空列表（非错误）。
//
// 解析复用 parseNumstatZ；二进制条目（'-' 计数）按既有 numstat 口径标记 IsBinary 且不计行。
// 结果 MUST 按路径 UTF-8 byte-wise 字典序排序后返回——parseNumstatZ 返回 map（遍历无序），
// MUST NOT 直接遍历输出。条目数超 MaxStatusFiles 返回 ErrTooManyFilesChanged。
//
// 只读操作，不进 repo 写锁。argv 数组、context 取消、有界读取复用 run()（exec.go）。
func BranchDiffFiles(ctx context.Context, dir, mbOID, headOID string) ([]BranchDiffFile, error) {
	out, _, err := run(ctx, dir, "diff", "--numstat", "-z", mbOID, headOID)
	if err != nil {
		return nil, err
	}

	byPath, _ := parseNumstatZ([]byte(out))
	if len(byPath) > MaxStatusFiles {
		return nil, ErrTooManyFilesChanged
	}

	files := make([]BranchDiffFile, 0, len(byPath))
	for path, e := range byPath {
		files = append(files, BranchDiffFile{
			Path:      path,
			Additions: e.additions,
			Deletions: e.deletions,
			IsBinary:  e.isBinary,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
