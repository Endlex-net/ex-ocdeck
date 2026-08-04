package git

import "errors"

// FileStatus 描述单个文件在 porcelain v2 中的状态条目。
type FileStatus struct {
	// Kind 为条目类型：'1' ordinary / '2' rename / 'u' unmerged / '?' untracked / '!' ignored。
	Kind string
	// Path 为条目对应的文件路径（rename 时为新路径/目标路径）。
	Path string
	// Rename 为 rename 条目的旧路径/来源路径；非 rename 为空。
	Rename string
	// XY 为两字符的 porcelain v2 状态码（已将 '.' 归一化为 ' '，untracked 为 "??"）。
	X byte
	Y byte
	// Staged 当 X != ' ' 且 X != '?' 时为 true（已暂存改动）。
	Staged bool
	// Unstaged 当 Y != ' ' 且 Y != '?' 时为 true（工作区改动）。
	Unstaged bool
	// Untracked 当条目为 '?' 时为 true。
	Untracked bool
	// Ignored 当条目为 '!'（--ignored=traditional 文件级枚举）时为 true。
	// 既有 Status 调用默认不含 ignored 条目；仅 ListIgnoredUntracked 返回。
	Ignored bool
	// Additions / Deletions 为关联 numstat 的增删行数（未跟踪文件 additions 表示行数估算）。
	Additions int
	Deletions int
	// IsBinary 当 numstat 报告二进制（'-' '-'）时为 true。
	IsBinary bool
}

// ErrTooManyFilesChanged 表示变更文件数超过上限。
var ErrTooManyFilesChanged = errors.New("too many changed files")

// MaxStatusFiles 变更文件数上限。
const MaxStatusFiles = 10000
