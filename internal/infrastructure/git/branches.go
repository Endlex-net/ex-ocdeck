package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ListBranches 列出 repoPath 仓库的分支短名，按"本地分支在前、远端分支在后"分组
// 稳定排序、合并后全局去重返回（add-plain-dir-project D10：GET /api/v1/projects/{id}/branches）。
//
// 排除远端 symbolic HEAD（如 `origin/HEAD -> origin/main`）：`git branch -r` 默认列出
// 这类 symbolic ref，但它不是可 checkout 的真实分支。按真实 symbolic 元数据过滤——远端分支
// 用 `--format=%(refname:lstrip=2)%09%(symref)`，仅排除 `%(symref)` 非空的条目（symbolic ref，
// 如 `origin/HEAD`）；真实远端分支 `origin/feature/HEAD`（symref 为空）保留。不用
// `%(refname:short)`：git 会把远端路径末段 `HEAD` 当成 symbolic 省略，把
// `origin/feature/HEAD` 收成 `origin/feature`。本地分支逻辑不变（本地分支非 symbolic，
// symref 恒空，不过滤；合法本地分支名 `feature/HEAD` 保留）。
//
// 去重为合并后全局去重：本地与远端同短名（如本地 `feature-x` 与远端 `feature-x`）合并后去重，
// 保留本地在前、远端在后的稳定顺序（本地条目先出现即保留，远端同短名条目丢弃）。
//
// 只读操作，不进 repo 写锁（与 Status/ListIgnoredUntracked 同）。argv 数组调用、context 取消、
// 有界读取复用 run()（exec.go）。repoPath 必须为 git 仓库；非仓库由调用方在 API 层按 kind 拒绝。
func ListBranches(ctx context.Context, repoPath string) ([]string, error) {
	localOut, _, err := run(ctx, repoPath, "branch", "--format=%(refname:lstrip=2)")
	if err != nil {
		return nil, fmt.Errorf("git branch (local): %w", err)
	}
	// 远端分支附带 symref 元数据：%(symref) 非空即为 symbolic ref（origin/HEAD），排除。
	remoteOut, _, err := run(ctx, repoPath, "branch", "-r", "--format=%(refname:lstrip=2)%09%(symref)")
	if err != nil {
		return nil, fmt.Errorf("git branch (remote): %w", err)
	}

	// 本地分支不过滤（合法分支名如 feature/HEAD）；仅去空行。
	local := parseLocalBranchLines(localOut)
	// 远端分支按 symref 元数据过滤 symbolic HEAD（origin/HEAD），保留真实分支（含 origin/feature/HEAD）。
	remote := parseRemoteBranchLines(remoteOut)

	// 各组内稳定排序。
	local = sortStable(local)
	remote = sortStable(remote)

	// 合并后全局去重：本地在前、远端在后，同短名保留首次出现（本地优先）。
	return dedupMerged(append(local, remote...)), nil
}

// symrefSep 是 refname:short 与 symref 之间的分隔符（%09 = TAB）。
const symrefSep = "\t"

// parseLocalBranchLines 按行切分本地 branch --format=%(refname:lstrip=2) 输出，去空行。
// 本地分支非 symbolic（symref 恒空），不过滤 HEAD；合法分支名 feature/HEAD 保留。
func parseLocalBranchLines(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// parseRemoteBranchLines 按行切分远端 `branch -r --format=%(refname:lstrip=2)%09%(symref)` 输出。
// 每行形如 `<short>\t<symref>`：symref 非空为 symbolic ref（origin/HEAD），排除；symref 为空为
// 真实远端分支（含 origin/feature/HEAD），保留。去空行。
func parseRemoteBranchLines(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 拆分短名与 symref：首个 TAB 之前为短名，之后为 symref（可能为空）。
		short, symref, _ := strings.Cut(line, symrefSep)
		short = strings.TrimSpace(short)
		if short == "" {
			continue
		}
		// symref 非空 → symbolic ref（如 origin/HEAD），排除。
		if strings.TrimSpace(symref) != "" {
			continue
		}
		names = append(names, short)
	}
	return names
}

func sortStable(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// dedupMerged 对合并切片（本地在前、远端在后）全局去重，保留首次出现的短名。
// 本地与远端同短名时本地条目先出现即保留，远端同短名条目丢弃，保持"本地优先"稳定顺序。
func dedupMerged(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}