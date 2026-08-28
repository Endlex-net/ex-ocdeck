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
	// Additions / Deletions 为关联 numstat 的增删行数。
	// 未跟踪文件（Untracked）无 numstat，Additions 为按行读取估算的文件行数（deletions=0），
	// IsBinary 由前 8000 字节 NUL 嗅探标记；单文件 16MB / 累计 64MB 预算耗尽时 Additions=0 但 IsBinary 仍标记。
	Additions int
	Deletions int
	// IsBinary 当 numstat 报告二进制（'-' '-'）时为 true。
	// 未跟踪文件经前 8000 字节 NUL 嗅探判定（对齐 git 启发式）。
	IsBinary bool
}

// ErrTooManyFilesChanged 表示变更文件数超过上限。
var ErrTooManyFilesChanged = errors.New("too many changed files")

// ErrInvalidDiffPath 表示 diff path 词法校验失败（空 / 绝对路径 / `..` 逃逸 / NUL）。
// Manager 阶段① 词法校验与工作区新侧读取同源调用 ValidateDiffPath，据此映射 invalid_input，
// MUST 用 errors.Is 判定，禁止字符串匹配。
var ErrInvalidDiffPath = errors.New("invalid diff path")

// ErrUnmergedPath 表示 index 中 path 无 stage-0 记录但存在同 path 其他 stage 记录（未解决冲突）。
// Manager 据此映射 invalid_state（MUST NOT 以「旧侧不存在」降级），errors.Is 判定。
var ErrUnmergedPath = errors.New("unmerged path in index")

// ErrWorktreeEscape 表示新侧工作区文件 resolve 后的真实路径越出 worktree 根（中间级 symlink 逃逸）。
// Manager 据此映射 invalid_input，errors.Is 判定。
var ErrWorktreeEscape = errors.New("worktree path escape")

// ErrSubmoduleDirtyProbe 表示子模块 dirty 探测（git status --porcelain）执行失败。
// Manager 据此映射 git_error 并透传 stderr（spec 错误映射：MUST NOT 静默按 clean 处理，
// 否则丢失 -dirty 可见语义），errors.Is 判定。
var ErrSubmoduleDirtyProbe = errors.New("submodule dirty probe failed")

// MaxStatusFiles 变更文件数上限。
const MaxStatusFiles = 10000
