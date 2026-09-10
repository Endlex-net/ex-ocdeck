// detail_extract.go：按 D5 提取表从请求 metadata 确定性提取判定详情
//（task-permission-mode 8.2）。纯函数、无 IO；本表（design.md D5）为跨 artifact
// 唯一矩阵——字段 JSON 路径/类型/必填/完整性条件、界值、UTF-8 安全截断、截断顺序
// 与遍历序、Degraded 三值归一化与优先级均按其执行。
//
// 内容/标记分离（F2）：字段级截断在 builder 中只记录截断状态与内容（不含标记），
// 8192 总量预算只计内容；全部截断/裁剪阶段完成后统一渲染时才追加标记。
// 状态与元素同一身份（F11）：files 成员的截断/关键性状态内嵌 fileEntry（随成员
// 一起 append/裁剪，无并行索引表——非法成员 continue 不会造成状态错位）。
package task

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// newDetailBuilder 构造空提取中间态。
func newDetailBuilder() *detailBuilder { return &detailBuilder{} }

// 证据降级原因三值（design.md D5：确定性降级）。
const (
	DegradedMissing   = "missing_critical"
	DegradedMalformed = "malformed_critical"
	DegradedTruncated = "truncated_critical"
	permTruncMarker   = "…[truncated]" // 标记自身不计入字段限额
	detailMaxCommand  = 2048           // command ≤ 2048 字节
	detailMaxDiff     = 4096           // 单文件 diff/patch ≤ 4096 字节
	detailMaxFiles    = 20             // files ≤ 20 个
	detailMaxTotal    = 8192           // Detail 全部提取字段字节总和 ≤ 8192 字节
	auditDetailMax    = 1024           // 审计 detail 最终串 ≤ 1024 字节（D11，含前缀与标记）
)

// RequestFileChange apply_patch 成员（D5：type → relativePath → patch → movePath）。
type RequestFileChange struct {
	Type         string `json:"type"`
	RelativePath string `json:"relativePath"`
	Patch        string `json:"patch"`
	MovePath     string `json:"movePath,omitempty"`
}

// RequestDetail 按类别从 metadata 提取的请求详情（观察层已归 nil 的 metadata 按
// missing 处理关键类别）。
type RequestDetail struct {
	Degraded string // missing_critical|malformed_critical|truncated_critical；空 = 无降级

	Command      string // bash / external_directory（shell 来源）
	Filepath     string // edit|write / external_directory（文件工具来源）
	Diff         string // edit|write
	Files        []RequestFileChange
	URL          string // webfetch
	Description  string // task
	SubagentType string // task
	Pattern      string // grep|glob
	Path         string // grep|glob
	Include      string // grep|glob
	ParentDir    string // external_directory
	Directories  []string // external_directory
}

// detailAcc 降级原因累积（全量扫描完成后按固定优先级归一，F1：归一化在全部
// 截断/裁剪阶段之后一次性写回）。
type detailAcc struct {
	missing   bool
	malformed bool
	truncated bool
}

func (a *detailAcc) markMissing()   { a.missing = true }
func (a *detailAcc) markMalformed() { a.malformed = true }
func (a *detailAcc) markTruncated() { a.truncated = true }

// normalize 依固定优先级归一 Degraded：malformed > missing > truncated。
func (a *detailAcc) normalize() string {
	switch {
	case a.malformed:
		return DegradedMalformed
	case a.missing:
		return DegradedMissing
	case a.truncated:
		return DegradedTruncated
	default:
		return ""
	}
}

// utf8SafeLimit 返回不超过 max 字节的 UTF-8 安全前缀长度。
func utf8SafeLimit(s string, max int) int {
	if max >= len(s) {
		return len(s)
	}
	limit := max
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return limit
}

// truncateUTF8 按字节限额 UTF-8 安全截断（不切断序列），追加标记（标记不计入限额）。
func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max <= 0 {
		return permTruncMarker, true
	}
	return s[:utf8SafeLimit(s, max)] + permTruncMarker, true
}

// rendered 截断状态渲染：trunc 时内容追加标记（F2：标记只在渲染时出现）。
func rendered(content string, trunc bool) string {
	if trunc {
		return content + permTruncMarker
	}
	return content
}

// strField 取 object 内 string 字段：返回 (值, 存在, 类型合法)。
func strField(obj map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return "", false, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", true, false
	}
	return s, true, true
}

// isCriticalCategory 关键证据类别（缺失/畸形/截断均强制转人工）。
func isCriticalCategory(permission string) bool {
	switch permission {
	case "bash", "edit", "write", "apply_patch", "external_directory":
		return true
	default:
		return false
	}
}

// fileEntry apply_patch 成员：内容与截断/关键状态同一身份（F11，随成员一起
// append/裁剪，无并行索引表）。tr* 为各字段截断状态；crit* 为关键性
//（type/relativePath/patch 恒关键；movePath 仅 move 关键，F10）。
type fileEntry struct {
	fc      RequestFileChange
	isMove  bool
	trType  bool
	trRel   bool
	trPatch bool
	trMove  bool
	critType  bool
	critRel   bool
	critPatch bool
	critMove  bool
}

// detailBuilder 提取中间态：标量字段内容/截断状态成对；总量预算只计内容，
// 渲染阶段统一追加标记（F2）。
type detailBuilder struct {
	acc detailAcc

	command, filepath, diff string
	trCommand, trFilepath, trDiff bool

	url, description, subagentType string
	trURL, trDescription, trSubagentType bool

	pattern, path, include string
	trPattern, trPath, trInclude bool

	parentDir  string
	trParentDir bool

	files []fileEntry
	dirs  []string
	dirsTrunc []bool
}

// required 类别关键字段（string，必填）：缺失 → missing_critical；类型错或空串 →
// malformed_critical；超字段级限额（max>0）→ 截断 + truncated_critical。
func (b *detailBuilder) required(p *string, tr *bool, obj map[string]json.RawMessage, key string, max int) {
	s, present, ok := strField(obj, key)
	switch {
	case !present:
		b.acc.markMissing()
		return
	case !ok || s == "":
		b.acc.markMalformed()
		return
	}
	if max > 0 {
		kept := utf8SafeLimit(s, max)
		if kept < len(s) {
			b.acc.markTruncated()
			*p = s[:kept]
			*tr = true
			return
		}
	}
	*p = s
}

// optionalDrop 可选字段（webfetch/task/grep/glob，F5）：缺失 → 空（patterns 已含
// 主要信息，不降级）；类型错 → 舍弃该字段（fallback 判定，不降级）；有值原样
//（受总量约束）。
func (b *detailBuilder) optionalDrop(p *string, obj map[string]json.RawMessage, key string) {
	s, present, ok := strField(obj, key)
	if !present || !ok {
		return
	}
	*p = s
}

// optionalStrict 来源字段（external_directory，F9）：可缺省（缺失 → 空、不降级），
// 但存在时必须合法——类型错 → malformed_critical；超字段级限额（max>0）→ 截断 +
// truncated_critical。
func (b *detailBuilder) optionalStrict(p *string, tr *bool, obj map[string]json.RawMessage, key string, max int) {
	s, present, ok := strField(obj, key)
	switch {
	case !present:
		return
	case !ok:
		b.acc.markMalformed()
		return
	}
	if max > 0 {
		kept := utf8SafeLimit(s, max)
		if kept < len(s) {
			b.acc.markTruncated()
			*p = s[:kept]
			*tr = true
			return
		}
	}
	*p = s
}

// optionalSlice 可选 []string（directories，F3/R1-F1）：缺失/null（整体）→ 空
//（不降级）；非数组 → malformed_critical；逐成员校验——**字面 null 与非 string
// 成员均视为非法**（null 解码到 string 不报错得到空串，会绕过确定性降级，R1-F1）→
// malformed_critical 且跳过该成员（合法成员保留，「合法与异常成员并存」）；空串
// 元素合法但不计入非空目标。
func (b *detailBuilder) optionalSlice(obj map[string]json.RawMessage, key string) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		b.acc.markMalformed()
		return
	}
	bad := false
	for _, e := range elems {
		if bytesEqualFold(e, "null") {
			// 字面 null 成员：解码到 string 不报错，必须显式拒绝（R1-F1）。
			bad = true
			continue
		}
		var s string
		if err := json.Unmarshal(e, &s); err != nil {
			// 非 string 成员（数字/布尔/对象/数组）。
			bad = true
			continue
		}
		b.dirs = append(b.dirs, s)
		b.dirsTrunc = append(b.dirsTrunc, false)
	}
	if bad {
		b.acc.markMalformed()
	}
}

// bytesEqualFold 判断 RawMessage 去除首尾空白后与字面值等价。
func bytesEqualFold(raw json.RawMessage, literal string) bool {
	t := strings.TrimSpace(string(raw))
	return t == literal
}

// applyPatchFiles apply_patch：files 数组必填、成员形状校验、数量裁剪 ≤20、成员
// patch 字段级截断 ≤4096。缺失 → missing；非数组/空数组/成员形状错误 → malformed；
// 数量裁剪或任一 patch 截断 → truncated。状态内嵌成员（F11）。
func (b *detailBuilder) applyPatchFiles(obj map[string]json.RawMessage) {
	raw, ok := obj["files"]
	if !ok || string(raw) == "null" {
		b.acc.markMissing()
		return
	}
	var members []struct {
		Type         *string `json:"type"`
		RelativePath *string `json:"relativePath"`
		Patch        *string `json:"patch"`
		MovePath     *string `json:"movePath"`
	}
	if err := json.Unmarshal(raw, &members); err != nil {
		b.acc.markMalformed()
		return
	}
	if len(members) == 0 {
		b.acc.markMalformed()
		return
	}
	validType := func(t string) bool {
		return t == "new" || t == "update" || t == "delete" || t == "move"
	}
	bad := false
	for _, m := range members {
		if m.Type == nil || !validType(*m.Type) {
			bad = true
			continue
		}
		if m.RelativePath == nil || *m.RelativePath == "" {
			bad = true
			continue
		}
		if m.Patch == nil && *m.Type != "move" {
			bad = true
			continue
		}
		if *m.Type == "move" && (m.MovePath == nil || *m.MovePath == "") {
			bad = true
			continue
		}
		fe := fileEntry{
			isMove:    *m.Type == "move",
			critType:  true,
			critRel:   true,
			critPatch: true,
			critMove:  *m.Type == "move", // F10：move 的 movePath 为关键槽
		}
		fe.fc.Type = *m.Type
		fe.fc.RelativePath = *m.RelativePath
		if m.Patch != nil && *m.Patch != "" {
			kept := utf8SafeLimit(*m.Patch, detailMaxDiff)
			if kept < len(*m.Patch) {
				b.acc.markTruncated()
				fe.trPatch = true
				fe.fc.Patch = (*m.Patch)[:kept]
			} else {
				fe.fc.Patch = *m.Patch
			}
		}
		if m.MovePath != nil {
			fe.fc.MovePath = *m.MovePath
		}
		b.files = append(b.files, fe)
	}
	if bad {
		b.acc.markMalformed()
	}
	// 数量裁剪（裁尾部，≤20）→ 关键证据丢失 → truncated。状态随成员一起裁剪（F11）。
	if len(b.files) > detailMaxFiles {
		b.files = b.files[:detailMaxFiles]
		b.acc.markTruncated()
	}
}

// extractRequestDetail 按类别提取（严格按 D5 提取表）。metadata 为观察层归一化产物
//（nil 或 JSON object）；nil 且关键类别 → missing_critical；非关键类别缺失以
// permission+patterns 判定（detail 为空）。Degraded 在全部截断/裁剪阶段完成后
// 最后归一（F1：总量裁剪触发的 truncated 必须回写）。
func extractRequestDetail(permission string, metadata json.RawMessage) RequestDetail {
	var d RequestDetail
	if metadata == nil {
		if isCriticalCategory(permission) {
			d.Degraded = DegradedMissing
		}
		return d
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &obj); err != nil {
		// 观察层已保证 object；此为防御（损坏），按 missing 归一。
		if isCriticalCategory(permission) {
			d.Degraded = DegradedMissing
		}
		return d
	}
	b := newDetailBuilder()
	switch permission {
	case "bash":
		b.required(&b.command, &b.trCommand, obj, "command", detailMaxCommand)
	case "edit", "write":
		b.required(&b.filepath, &b.trFilepath, obj, "filepath", 0)
		b.required(&b.diff, &b.trDiff, obj, "diff", detailMaxDiff)
	case "apply_patch":
		b.applyPatchFiles(obj)
	case "webfetch":
		b.optionalDrop(&b.url, obj, "url")
	case "task":
		b.optionalDrop(&b.description, obj, "description")
		b.optionalDrop(&b.subagentType, obj, "subagent_type")
	case "grep", "glob":
		b.optionalDrop(&b.pattern, obj, "pattern")
		b.optionalDrop(&b.path, obj, "path")
		b.optionalDrop(&b.include, obj, "include")
	case "read":
		// patterns 即路径，无提取字段。
	case "external_directory":
		// F3：shell 来源（command，≤2048B 截断）与文件来源（filepath/parentDir/
		// directories）分别提取、可并存（F9：可缺省但存在时必须合法）；统一完整性
		// 检查：至少一个非空目标。
		b.optionalStrict(&b.command, &b.trCommand, obj, "command", detailMaxCommand)
		b.optionalStrict(&b.filepath, &b.trFilepath, obj, "filepath", 0)
		b.optionalStrict(&b.parentDir, &b.trParentDir, obj, "parentDir", 0)
		b.optionalSlice(obj, "directories")
		// F3：至少一个非空目标——空串目录元素不算目标。
		hasNonEmptyDir := false
		for _, dir := range b.dirs {
			if dir != "" {
				hasNonEmptyDir = true
				break
			}
		}
		hasTarget := b.command != "" || b.filepath != "" || b.parentDir != "" || hasNonEmptyDir
		if !hasTarget {
			b.acc.markMissing()
		}
	default:
		// 未识别类别：仅 permission+patterns。
	}
	b.totalTrim()
	b.render(&d)
	// F1：归一化必须是最后一步——总量裁剪触发的 truncated 同样回写 Degraded。
	d.Degraded = b.acc.normalize()
	return d
}

// ExtractRequestDetail 组合根/集成验证用的导出薄封装（语义同 extractRequestDetail；
// 内部消费路径经 judgeAndReply 调用未导出版本，保证判定与审计同源）。
func ExtractRequestDetail(permission string, metadata json.RawMessage) RequestDetail {
	return extractRequestDetail(permission, metadata)
}

// totalTrim 总量裁剪（≤8192 字节内容预算，按 D5 遍历序从末尾字符串槽逆向缩减；
// 预算不足以保留整个槽时对该槽执行字段级截断）。内容/标记分离（F2）：预算只计
// 内容；关键槽（关键字段）被波及 → truncated_critical（F1：最后归一回写）。
func (b *detailBuilder) totalTrim() {
	type slot struct {
		get      func() string
		set      func(string)
		trunc    *bool
		critical bool
	}
	var slots []slot
	add := func(get func() string, set func(string), trunc *bool, critical bool) {
		if v := get(); v != "" {
			slots = append(slots, slot{get: get, set: set, trunc: trunc, critical: critical})
		}
	}
	add(func() string { return b.command }, func(s string) { b.command = s }, &b.trCommand, true)
	add(func() string { return b.filepath }, func(s string) { b.filepath = s }, &b.trFilepath, true)
	add(func() string { return b.diff }, func(s string) { b.diff = s }, &b.trDiff, true)
	add(func() string { return b.url }, func(s string) { b.url = s }, &b.trURL, false)
	add(func() string { return b.description }, func(s string) { b.description = s }, &b.trDescription, false)
	add(func() string { return b.subagentType }, func(s string) { b.subagentType = s }, &b.trSubagentType, false)
	add(func() string { return b.pattern }, func(s string) { b.pattern = s }, &b.trPattern, false)
	add(func() string { return b.path }, func(s string) { b.path = s }, &b.trPath, false)
	add(func() string { return b.include }, func(s string) { b.include = s }, &b.trInclude, false)
	add(func() string { return b.parentDir }, func(s string) { b.parentDir = s }, &b.trParentDir, true)
	for i := range b.files {
		fe := &b.files[i]
		add(func() string { return fe.fc.Type }, func(s string) { fe.fc.Type = s }, &fe.trType, fe.critType)
		add(func() string { return fe.fc.RelativePath }, func(s string) { fe.fc.RelativePath = s }, &fe.trRel, fe.critRel)
		add(func() string { return fe.fc.Patch }, func(s string) { fe.fc.Patch = s }, &fe.trPatch, fe.critPatch)
		add(func() string { return fe.fc.MovePath }, func(s string) { fe.fc.MovePath = s }, &fe.trMove, fe.critMove)
	}
	for i := range b.dirs {
		i := i
		add(func() string { return b.dirs[i] }, func(s string) { b.dirs[i] = s }, &b.dirsTrunc[i], true)
	}

	total := 0
	for _, s := range slots {
		total += len(s.get())
	}
	if total <= detailMaxTotal {
		return
	}
	// 从末尾逆向缩减：预算只计内容（标记渲染时追加，不计入）。
	for i := len(slots) - 1; i >= 0 && total > detailMaxTotal; i-- {
		s := slots[i]
		cur := s.get()
		budget := detailMaxTotal - (total - len(cur))
		if budget <= 0 {
			// 无预算保留任何内容：内容丢失（渲染时留标记提示）。
			total -= len(cur)
			s.set("")
			*s.trunc = true
			b.markTruncatedIf(s.critical)
			continue
		}
		kept := utf8SafeLimit(cur, budget)
		s.set(cur[:kept])
		total -= len(cur) - kept
		*s.trunc = true
		b.markTruncatedIf(s.critical)
	}
}

// markTruncatedIf 关键槽被总量裁剪波及时标记 truncated（非关键槽照常判定）。
func (b *detailBuilder) markTruncatedIf(critical bool) {
	if critical {
		b.acc.markTruncated()
	}
}

// render 将 builder 内容写入 RequestDetail：截断过的槽渲染时追加标记
//（总量预算只计内容，F2）。
func (b *detailBuilder) render(d *RequestDetail) {
	d.Command = rendered(b.command, b.trCommand)
	d.Filepath = rendered(b.filepath, b.trFilepath)
	d.Diff = rendered(b.diff, b.trDiff)
	d.URL = rendered(b.url, b.trURL)
	d.Description = rendered(b.description, b.trDescription)
	d.SubagentType = rendered(b.subagentType, b.trSubagentType)
	d.Pattern = rendered(b.pattern, b.trPattern)
	d.Path = rendered(b.path, b.trPath)
	d.Include = rendered(b.include, b.trInclude)
	d.ParentDir = rendered(b.parentDir, b.trParentDir)
	d.Directories = make([]string, len(b.dirs))
	for i := range b.dirs {
		d.Directories[i] = rendered(b.dirs[i], b.dirsTrunc[i])
	}
	d.Files = make([]RequestFileChange, len(b.files))
	for i := range b.files {
		fe := &b.files[i]
		d.Files[i] = RequestFileChange{
			Type:         rendered(fe.fc.Type, fe.trType),
			RelativePath: rendered(fe.fc.RelativePath, fe.trRel),
			Patch:        rendered(fe.fc.Patch, fe.trPatch),
			MovePath:     rendered(fe.fc.MovePath, fe.trMove),
		}
	}
}
