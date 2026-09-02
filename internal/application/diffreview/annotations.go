// annotations.go 实现 3.5 批注用例（design.md D4 快照/stale + spec 批注创建/列表/编辑/删除）。
//
// 用例：
//   - CreateAnnotation：来源组合约束（untracked→ref空+side new）、1-based 闭区间、窗口自洽校验 → 落库。
//   - ListAnnotations：D4 stale 惰性计算（同源重读全窗口比对、截断窗口不触及末尾不完整元素保持 active、
//     读取失败=stale 不阻断）。
//   - UpdateComment：空白拒绝/同值 revision 不变（store 三态原语直接用）。
//   - DeleteAnnotation。
package diffreview

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// AnnotationView 为 ListAnnotations 返回的单条批注视图（含 stale 惰性计算结果）。
type AnnotationView struct {
	DiffAnnotationRecord
	Stale bool
}

// CreateAnnotation 创建批注（design.md D4 来源组合约束 + spec 批注创建）。
//
// 校验顺序（落库前，任一失败零副作用）。纯 DTO/领域校验先于任何 port 调用（F8/D8）：
//  1. 纯领域校验（无副作用、无 port 调用）：comment 非空白、来源组合约束、1-based 闭区间、
//     窗口自洽、path 非空、文本字段 ≤65536 UTF-8 bytes、snapshot 行数自洽。
//  2. 任务作用域准入（scope.Lookup：not_found/dir/unknown kind）。
//  3. 落库。
//
// comment 空白拒绝（spec「未输入任何评论不留存」后端侧落实，F8）；UpdateComment 仍单独
// 在 PATCH 时拒绝空白（同校验）。
func (s *Service) CreateAnnotation(ctx context.Context, in CreateDiffAnnotationInput) (DiffAnnotationRecord, error) {
	// F8：纯领域校验先于任何 port 调用（D8 共用校验：纯 DTO 校验置于 port 调用前）。
	if err := validateAnnotationInput(in); err != nil {
		return DiffAnnotationRecord{}, err
	}
	if err := s.checkTaskScope(ctx, in.TaskID); err != nil {
		return DiffAnnotationRecord{}, err
	}
	if err := s.repo.CreateDiffAnnotation(ctx, in); err != nil {
		return DiffAnnotationRecord{}, err
	}
	// store 原语 CreateDiffAnnotation 不回写行；读回以获得 revision(=1)/created_at/updated_at。
	rec, err := s.repo.GetDiffAnnotation(ctx, in.ID)
	if err != nil {
		return DiffAnnotationRecord{}, err
	}
	return rec, nil
}

// ListAnnotations 列出任务全部活动批注并惰性计算 stale（design.md D4 + spec 批注锚定状态）。
//
// stale 规则（D4 唯一）：
//   - 按来源元组(ref/untracked/side)经 DiffSourcePort 重读该侧有界内容。
//   - 取 [snapshotStartLine, snapshotStartLine+snapshotLineCount) 行窗口与 snapshot 全文比对，不等→stale=true。
//   - 截断文件窗口结束于末尾不完整元素之前→正常比对保持 active；窗口覆盖末尾不完整元素或
//     无法完整取得→stale=true。
//   - 读取失败 MUST NOT 报错阻断列表（该批注 stale=true 但列表正常返回）。
//   - stale 仅计算返回，不落库。
func (s *Service) ListAnnotations(ctx context.Context, taskID string) ([]AnnotationView, error) {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return nil, err
	}
	recs, err := s.repo.ListDiffAnnotationsByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	views := make([]AnnotationView, len(recs))
	for i, rec := range recs {
		views[i] = AnnotationView{
			DiffAnnotationRecord: rec,
			Stale:                s.computeStale(ctx, taskID, rec),
		}
	}
	return views, nil
}

// UpdateComment 编辑批注评论（spec 编辑批注评论 scenario + design.md D4）。
//
// 规则：
//   - comment trim 后空白 → ErrEmptyComment（invalid_input）。
//   - 同值 revision 不变：store 原语 UpdateDiffAnnotationComment 返回三态，
//     Matched=true && Changed=false 时直接返回当前行（revision 不递增）。
//   - 未命中（Matched=false）→ ErrAnnotationNotFound（not_found）。
//   - 真实变更（Changed=true）→ 读回新行（revision 已 +1）。
//
// F4：单记录操作先做归属校验。先 GetDiffAnnotation 取行，校验 rec.TaskID==taskID
// （归属不符统一 not_found，不泄露跨任务批注存在性）；归属不符零副作用（不写评论）。
// F8：comment 空白与 65536-byte 上限为纯 DTO 校验，先于 port 调用（D8 共用校验置于 port 调用前）。
func (s *Service) UpdateComment(ctx context.Context, taskID, annotationID, comment string) (DiffAnnotationRecord, error) {
	if strings.TrimSpace(comment) == "" {
		return DiffAnnotationRecord{}, ErrEmptyComment
	}
	// F8/D8：comment UTF-8 decoded bytes ≤65536。
	if len(comment) > maxTextFieldBytes {
		return DiffAnnotationRecord{}, ErrFieldTooLarge
	}
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return DiffAnnotationRecord{}, err
	}
	// F4：归属校验。先取行确认属于本任务，归属不符统一 not_found（零写副作用）。
	rec, err := s.repo.GetDiffAnnotation(ctx, annotationID)
	if err != nil {
		return DiffAnnotationRecord{}, err
	}
	if rec.TaskID != taskID {
		return DiffAnnotationRecord{}, ErrAnnotationNotFound
	}
	res, err := s.repo.UpdateDiffAnnotationComment(ctx, annotationID, comment)
	if err != nil {
		return DiffAnnotationRecord{}, err
	}
	if !res.Matched {
		return DiffAnnotationRecord{}, ErrAnnotationNotFound
	}
	if !res.Changed {
		// 同值命中：revision 不变，返回当前行。
		return s.repo.GetDiffAnnotation(ctx, annotationID)
	}
	return s.repo.GetDiffAnnotation(ctx, annotationID)
}

// DeleteAnnotation 删除批注（spec 删除批注无撤回）。affected=0 视为 not_found。
//
// F4：先做归属校验。GetDiffAnnotation 取行校验 rec.TaskID==taskID，归属不符统一 not_found
// （零删除副作用，不泄露跨任务批注存在性）。
func (s *Service) DeleteAnnotation(ctx context.Context, taskID, annotationID string) error {
	if err := s.checkTaskScope(ctx, taskID); err != nil {
		return err
	}
	// F4：归属校验。先取行确认属于本任务，归属不符统一 not_found（零删除副作用）。
	rec, err := s.repo.GetDiffAnnotation(ctx, annotationID)
	if err != nil {
		return err
	}
	if rec.TaskID != taskID {
		return ErrAnnotationNotFound
	}
	n, err := s.repo.DeleteDiffAnnotation(ctx, annotationID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAnnotationNotFound
	}
	return nil
}

// --- 校验 helper（design.md D4 来源组合约束 + 窗口自洽） ---

// maxTextFieldBytes 为单文本字段（snapshot/comment/note）的 UTF-8 decoded bytes 上限
// （design.md D8 line 274：snapshot/comment/note 各 ≤65536 UTF-8 bytes）。
const maxTextFieldBytes = 65536

// validateAnnotationInput 执行 CreateAnnotation 的纯领域校验（F8/D8：无副作用、无 port 调用，
// 先于任何 port 调用完成）。任一失败零副作用。
//   - path 词法校验（拒绝空、绝对路径、`..` 逃逸、NUL——与 git.ValidateDiffPath 同源规则，
//     application 层本地实现，MUST NOT import git 包，分层约束）。
//   - comment trim 后非空白（spec「未输入任何评论不留存」后端侧落实，F8）。
//   - 来源组合约束（D4）。
//   - 1-based 闭区间。
//   - 窗口自洽 + snapshot 行数自洽（split('\n') 行模型，F9）。
//   - snapshot/comment 各 ≤65536 UTF-8 bytes（D8）。
func validateAnnotationInput(in CreateDiffAnnotationInput) error {
	if err := validateDiffPath(in.Path); err != nil {
		return err
	}
	if strings.TrimSpace(in.Comment) == "" {
		return ErrEmptyComment
	}
	if err := validateAnnotationSource(in.Side, in.Ref, in.Untracked); err != nil {
		return err
	}
	if err := validateAnnotationRange(in.StartLine, in.EndLine); err != nil {
		return err
	}
	if err := validateSnapshotWindow(in.StartLine, in.EndLine, in.SnapshotStartLine, in.SnapshotLineCount); err != nil {
		return err
	}
	// F9：snapshot 行数自洽。split('\n') 行模型：snapshot 的逻辑行数必须等于 snapshotLineCount。
	if got := countLogicalLines(in.Snapshot); got != in.SnapshotLineCount {
		return ErrInvalidSnapshotWindow
	}
	// D8：文本字段 UTF-8 decoded bytes 上限。
	if utf8.RuneCountInString(in.Snapshot) > 0 && len(in.Snapshot) > maxTextFieldBytes {
		return ErrFieldTooLarge
	}
	if len(in.Comment) > maxTextFieldBytes {
		return ErrFieldTooLarge
	}
	return nil
}

// validateDiffPath 校验 diff path 词法（F8：与 git.ValidateDiffPath 同源规则，application 层本地实现，
// MUST NOT import git 包——分层约束）。拒绝空、绝对路径、`..` 逃逸、NUL。
func validateDiffPath(path string) error {
	if path == "" {
		return ErrInvalidAnnotationPath
	}
	if filepath.IsAbs(path) {
		return ErrInvalidAnnotationPath
	}
	if strings.ContainsRune(path, 0) {
		return ErrInvalidAnnotationPath
	}
	// filepath.Clean 归一后检查是否逃逸到父目录（".." 或以 "../" 开头）。
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ErrInvalidAnnotationPath
	}
	return nil
}

// countLogicalLines 按 CM split('\n') 行模型计算逻辑行数（design.md D4 line 149：
// CM Text.of(split('\n')) 同一形态）。JS 语义：
//   - "a\n"  → split = ["a",""] → 2 行
//   - "a"    → split = ["a"]   → 1 行
//   - "a\nb" → split = ["a","b"] → 2 行
//   - ""     → split = [""]    → 1 行（空内容算 1 行空逻辑行）
//
// 保留 trailing empty logical line（与前端 CM 一致）。
func countLogicalLines(s string) int {
	// JS "".split('\n') = [""] → 1 行；Go strings.Split("", "\n") = [""]（1 元素）。
	return len(strings.Split(s, "\n"))
}

// side∈{old,new}；untracked=true→ref="" 且 side="new"；untracked=false→side 任意、ref 可空。
func validateAnnotationSource(side, ref string, untracked bool) error {
	if side != "old" && side != "new" {
		return ErrInvalidAnnotationSide
	}
	if untracked {
		if ref != "" {
			return ErrInvalidAnnotationSource
		}
		if side != "new" {
			return ErrInvalidAnnotationSource
		}
	}
	return nil
}

// validateAnnotationRange 校验 1-based 闭区间（spec 批注创建：startLine≥1, endLine≥startLine）。
func validateAnnotationRange(startLine, endLine int) error {
	if startLine < 1 {
		return ErrInvalidAnnotationRange
	}
	if endLine < startLine {
		return ErrInvalidAnnotationRange
	}
	return nil
}

// validateSnapshotWindow 校验窗口自洽（design.md D4：start/end 落在窗口内且 1-based 关系自洽）。
// 窗口 = [snapshotStartLine, snapshotStartLine + snapshotLineCount - 1]。
func validateSnapshotWindow(startLine, endLine, snapshotStartLine, snapshotLineCount int) error {
	if snapshotStartLine < 1 {
		return ErrInvalidSnapshotWindow
	}
	if snapshotLineCount < 1 {
		return ErrInvalidSnapshotWindow
	}
	windowEnd := snapshotStartLine + snapshotLineCount - 1
	if startLine < snapshotStartLine || endLine > windowEnd {
		return ErrInvalidSnapshotWindow
	}
	return nil
}

// computeStale 惰性计算单条批注的 stale 状态（design.md D4）。
// 读取失败 → stale=true（不阻断列表）。窗口无法完整取得 → stale=true。
//
// F9：使用对应侧各自的 truncation 标志（OldTruncated/NewTruncated），不再用聚合 result.Truncated。
// 仅当对应侧被截断且窗口覆盖其不完整末元素才 stale（无论前缀是否以 \n 结尾）；
// 另一侧截断不影响本侧 stale 判定（旧实现用聚合值导致仅另一侧截断+本侧天然无末尾换行误判 stale）。
func (s *Service) computeStale(ctx context.Context, taskID string, rec DiffAnnotationRecord) bool {
	// untracked 批注读 new 侧；tracked 批注按 side 读对应侧。
	// DiffSourcePort.Read 以 (ref,path,untracked) 三元组读取两侧内容；stale 比对按 side 取对应侧。
	src := DiffSource{Ref: rec.Ref, Path: rec.Path, Untracked: rec.Untracked}
	result, err := s.diff.Read(ctx, taskID, src)
	if err != nil {
		// 读取失败 MUST NOT 阻断列表（D4）：该批注 stale=true。
		return true
	}
	var content string
	var exists bool
	var sideTruncated bool
	if rec.Side == "new" {
		content = result.NewContent
		exists = result.NewExists
		sideTruncated = result.NewTruncated
	} else {
		content = result.OldContent
		exists = result.OldExists
		sideTruncated = result.OldTruncated
	}
	if !exists {
		// 对应侧不存在（文件删除/侧缺失）→ 窗口无法取得 → stale=true。
		return true
	}
	window, ok := extractLineWindow(content, rec.SnapshotStartLine, rec.SnapshotLineCount)
	if !ok {
		// 窗口无法完整取得（行数不足）→ stale=true。
		// F9：行数不足包括「截断点在窗口内」——截断后规范化内容行数少于窗口所需行数。
		return true
	}
	// F9：仅对应侧截断且窗口覆盖其不完整末元素 → stale。
	// D4（CM split('\n') 行模型）：某侧 truncated 时，该侧最后一个 split('\n') 元素视为不完整
	//（截断点落在它上面，含前缀恰以 \n 结尾时的末元素 ""——真实内容可能为 "a\nrest" 截成 "a\n"）；
	// 窗口结束在此不完整元素之前的完整行 → 正常比对（active）；窗口覆盖该不完整元素 → stale。
	// 另一侧截断不影响本侧 stale 判定。
	if sideTruncated {
		if isWindowTouchingIncompleteTruncationTail(content, rec.SnapshotStartLine, rec.SnapshotLineCount) {
			return true
		}
	}
	return window != rec.Snapshot
}

// isWindowTouchingIncompleteTruncationTail 判定窗口是否触及截断的不完整后缀（F9/D4）。
// content 为截断后的有界前缀。截断侧最后一个 split('\n') 元素都视为不完整（截断点落在它上面，
// 其后可能有被丢弃的内容）——无论 content 是否以 \n 结尾：前缀恰以 \n 结尾时，末元素 "" 仍是
// 未知 rest 的前缀（真实内容 "a\nrest" 被截成 "a\n" 时，末元素对应行可能实为 "rest"）。
// 窗口覆盖最后元素 → stale；窗口结束于之前的完整行 → 正常比对 active。
func isWindowTouchingIncompleteTruncationTail(content string, startLine, lineCount int) bool {
	lastIdx := len(splitLogicalLines(content)) - 1
	windowEnd := startLine - 1 + lineCount
	return windowEnd > lastIdx
}

// extractLineWindow 从 content 中提取 [startLine, startLine+lineCount) 的 1-based 行窗口全文。
// 返回 (window, true) 表示窗口完整取得；(window, false) 表示行数不足（窗口无法完整取得）。
// 行分隔按 \n，保留行尾 \r（CRLF 文件快照不漂移，design.md D4/F15）。
//
// F9：行模型与前端快照构造一致（design.md D4 line 149：CM Text.of(split('\n')) 同一形态）。
// splitLogicalLines 保留 trailing empty logical line（JS split('\n') 语义）；各元素为不含 \n 的
// 行片段（含前缀 \r），窗口 join 时以 \n 重连接（与原始 \n 分隔一致）。
func extractLineWindow(content string, startLine, lineCount int) (string, bool) {
	if startLine < 1 || lineCount < 1 {
		return "", false
	}
	lines := splitLogicalLines(content)
	if startLine-1+lineCount > len(lines) {
		return "", false
	}
	window := strings.Join(lines[startLine-1:startLine-1+lineCount], "\n")
	return window, true
}

// splitLogicalLines 按 CM split('\n') 行模型分行（design.md D4 line 149：Text.of(split('\n')) 同一形态）。
// JS 语义保留 trailing empty logical line：
//   - "a\n"  → ["a",""]（2 行：第 1 行 "a"，第 2 行 "" 空行）
//   - "a"    → ["a"]（1 行）
//   - "a\nb" → ["a","b"]（2 行）
//   - ""     → [""]（1 行空逻辑行）
//
// 各元素为不含 \n 的行片段（保留前缀 \r：CRLF 文件的行尾 \r 留在元素中，窗口 join 时以 \n 重连）。
// 与 D7 payload formatter 的 SplitAfter 规则无关（D7 只管 payload 格式化，不管 stale 行模型）。
func splitLogicalLines(s string) []string {
	return strings.Split(s, "\n")
}

// checkTaskScope 执行任务作用域准入（design.md D9：任务不存在→not_found, dir→invalid_input, 未知→internal）。
func (s *Service) checkTaskScope(ctx context.Context, taskID string) error {
	scope, err := s.scope.Lookup(ctx, taskID)
	if err != nil {
		return err
	}
	if !scope.Found {
		return ErrTaskNotFound
	}
	switch scope.Kind {
	case "repo":
		return nil
	case "dir":
		return ErrDirProject
	default:
		return ErrUnknownProjectKind
	}
}

// --- 额外 domain 错误（批注用例准入） ---

// ErrInvalidAnnotationSide side 非 old/new（来源组合约束，D4）。
var ErrInvalidAnnotationSide = errors.New("diffreview: invalid annotation side (must be old or new)")

// ErrInvalidAnnotationSource 来源组合非法（untracked 但 ref 非空或 side 非 new，D4）。
var ErrInvalidAnnotationSource = errors.New("diffreview: invalid annotation source combo (untracked requires empty ref and side=new)")

// ErrInvalidAnnotationRange 行范围非法（非 1-based 闭区间，spec 批注创建）。
var ErrInvalidAnnotationRange = errors.New("diffreview: invalid annotation line range (1-based closed interval)")

// ErrInvalidSnapshotWindow 快照窗口自洽失败（start/end 不落在窗口内，D4）。
var ErrInvalidSnapshotWindow = errors.New("diffreview: snapshot window not self-consistent with line range")

// ErrEmptyComment PATCH 评论 trim 后空白（spec 编辑批注评论）。
var ErrEmptyComment = errors.New("diffreview: annotation comment is empty after trim")

// ErrAnnotationNotFound 批注不存在（not_found，spec 编辑/删除）。
var ErrAnnotationNotFound = errors.New("diffreview: annotation not found")

// ErrInvalidAnnotationPath 批注 path 为空（spec 批注创建 path 校验，F8）。
var ErrInvalidAnnotationPath = errors.New("diffreview: annotation path is empty")

// ErrFieldTooLarge 单文本字段超过 65536 UTF-8 bytes 上限（design.md D8，F8）。
var ErrFieldTooLarge = errors.New("diffreview: text field exceeds 65536 UTF-8 bytes limit")
