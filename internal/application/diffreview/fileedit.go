// fileedit.go 实现 diff-review-workbench 文件编辑读取（tasks 3.9 / design.md D5 判别联合读取）
// 与写回（tasks 3.10 / design.md D5 固定顺序原子写）的 application 用例与 domain 类型。
//
// 分层（design.md D9）：本文件定义 domain 类型、FileEditPort（consumer-owned，task 层 adapter
// 实现）与 Service 上的用例方法。Service 负责领域逻辑（reasonCode 判定、请求字段格式校验、
// BOM 推导、换行重建）；FileEditPort adapter 负责任务锁、task/worktree/repo 校验、文件系统
// 禁锢/读写/临时文件/rename（见 internal/task/diffreview_fileedit.go）。
//
// 重建相关的纯函数（DeriveBOM / RebuildWriteBytes / DetectLineEnding / IsBinaryBytes）
// 在本包定义，由 adapter 调用以保持领域逻辑归属（adapter 仅协调文件系统操作）。
package diffreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// --- 常量（design.md D5 逐字） ---

// FileEditMaxBytes 编辑读取内容上限（design.md D5：512KiB = 524288 bytes）。
const FileEditMaxBytes = 512 * 1024

// binarySniffBytes 二进制嗅探窗口（与 git 包同口径，design.md D5）。
const binarySniffBytes = 8000

// utf8BOM 是 UTF-8 BOM 字节序列（EF BB BF）。
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// --- domain 类型：读取判别联合（design.md D5） ---

// ReadReasonCode 不可编辑原因枚举（design.md D5 逐字七值）。
type ReadReasonCode string

const (
	ReasonBinary           ReadReasonCode = "binary"
	ReasonNonUTF8          ReadReasonCode = "non_utf8"
	ReasonMixedLineEndings ReadReasonCode = "mixed_line_endings"
	ReasonTooLarge         ReadReasonCode = "too_large"
	ReasonNotRegular       ReadReasonCode = "not_regular"
	ReasonMissing          ReadReasonCode = "missing"
	ReasonReadOnly         ReadReasonCode = "read_only"
)

// LineEnding 换行风格枚举（design.md D5）。
type LineEnding string

const (
	LineEndingLF   LineEnding = "lf"
	LineEndingCRLF LineEnding = "crlf"
)

// FileEditReadResult 为 Service.ReadFile 的判别联合返回（design.md D5）。
//
// editable=true 时填充 Content/BaseHash/LineEnding/HasBOM/Mode；
// editable=false 时填充 ReasonCode/Reason。两组字段互斥。
type FileEditReadResult struct {
	Editable bool
	// editable=true 字段
	Content    string // 去除 UTF-8 BOM、CRLF 归一为 \n 后的文本
	BaseHash   string // 当前文件精确字节（含 BOM、原始换行）的 SHA-256 小写 hex
	LineEnding LineEnding
	HasBOM     bool
	Mode       string // 当前文件完整 chmod 值四位八进制字符串 0000..7777（含特殊位）
	// editable=false 字段
	ReasonCode ReadReasonCode
	Reason     string
}

// FileEditWriteRequest 为 Service.WriteFile 的请求（已解码 DTO，design.md D5 写回 body）。
type FileEditWriteRequest struct {
	Path       string
	Content    string // MUST 仅以 \n 为换行（含任何 \r → invalid_input）
	BaseHash   string // MUST 64 位小写 hex
	LineEnding LineEnding
	BaseMode   string // MUST 四位八进制字符串 0000..7777（含特殊位）
}

// FileEditWriteResult 为 Service.WriteFile 的返回（design.md D5 步骤 9）。
type FileEditWriteResult struct {
	BaseHash string // 新精确字节 hash
}

// --- FileEditPort（consumer-owned，task 层 adapter 实现，design.md D9） ---
//
// 读取：ReadRaw 返回常规文件的原始字节 + 完整 chmod 值；非常规/缺失 → Exists=false。
// adapter 负责任务锁、task/worktree/repo 校验、禁锢校验、Lstat 类型判定、有界读取。
// Service 据此做 reasonCode 判定（binary/non_utf8/mixed_line_endings/too_large/read_only）。
//
// 写回：Write 执行 design.md D5 步骤 2-9（adapter 持锁内完成）。Service 预先完成步骤 1
//（领域格式校验）。adapter 在步骤 4-5 中调用本包纯函数（DetectLineEnding/DeriveBOM/
// RebuildWriteBytes/SHA256Hex）以保持领域逻辑归属。

// FileEditRawFile 为 FileEditPort.ReadRaw 的返回：常规文件的原始读取结果。
type FileEditRawFile struct {
	// Exists 表示 Lstat 命中且为 regular file 且通过禁锢校验。false 表示缺失或非 regular
	//（不区分二者——Service 对 not_regular/missing 的区分由 adapter 经 error 返回，见下）。
	Exists bool
	// Mode 为 Lstat info.Mode() 的完整 chmod 值（含 setuid/setgid/sticky），uint32 形式。
	// Exists=false 时为 0。
	Mode uint32
	// Bytes 为原始精确字节（至多 FileEditMaxBytes+1 字节，+1 供 Service 判定 too_large）。
	// Exists=false 时为 nil。
	Bytes []byte
}

// FileEditReadRawError 描述 ReadRaw 时 Lstat 命中但非 regular（not_regular）与
// 完全缺失（missing）的区分（design.md D5：reasonCode not_regular vs missing）。
// adapter 将其作为 error 返回（errors.As 可判），Service 映射为对应 ReasonCode。
type FileEditReadRawError struct {
	NotRegular bool // true=非 regular 文件（symlink/dir/fifo 等）；false=缺失
}

func (e *FileEditReadRawError) Error() string {
	if e.NotRegular {
		return "diffreview: not a regular file"
	}
	return "diffreview: file missing"
}

// FileEditPort 表达文件编辑读写的文件系统能力（design.md D9，task 层 adapter 实现）。
type FileEditPort interface {
	// ReadRaw 读取 taskID worktree 中 path 的常规文件原始字节 + 完整 mode。
	// 任务锁冲突 → conflict（OpError 风格，由 adapter 映射）；任务不存在 → not_found；
	// worktree 空/非 repo → invalid_state/invalid_input；禁锢逃逸 → invalid_input；
	// 文件缺失 → 返回 FileEditRawFile{Exists:false}（非 error）；
	// 非 regular 文件 → 返回 *FileEditReadRawError{NotRegular:true}（供 Service 映射 not_regular）；
	// IO 错误 → internal。
	ReadRaw(ctx context.Context, taskID, path string) (FileEditRawFile, error)

	// Write 执行 design.md D5 步骤 2-9 的文件系统操作（adapter 持任务锁内完成）。
	// 步骤 1（领域格式校验）由 Service 在调用前完成。adapter 在步骤 4-5 调用本包纯函数
	//（DetectLineEnding/DeriveBOM/RebuildWriteBytes/SHA256Hex）完成领域计算。
	// 错误映射：任务锁冲突/conflict（hash/换行/mode 变化/终检失败）→ conflict；
	// 任务不存在 → not_found；worktree 空 → invalid_state；非 repo → invalid_input；
	// 禁锢逃逸/请求格式（adapter 侧二次防御）→ invalid_input；IO → internal。
	Write(ctx context.Context, taskID string, req FileEditWriteRequest) (FileEditWriteResult, error)
}

// --- 领域纯函数（design.md D5，由 Service 与 adapter 共用，领域逻辑归属本包） ---

// IsBinaryBytes 判定内容前 binarySniffBytes 字节是否含 NUL（design.md D5 二进制嗅探）。
func IsBinaryBytes(b []byte) bool {
	prefix := b
	if len(prefix) > binarySniffBytes {
		prefix = prefix[:binarySniffBytes]
	}
	return indexByte(prefix, 0) >= 0
}

// indexByte 在 b 中查找 c 的位置（避免引入 bytes 包依赖 strings.IndexByte 即可，
// 但此处直接手写避免对 []byte 的 strings.IndexByte 转换）。
func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// DetectLineEnding 从原始字节检测换行风格（design.md D5）。
// 无换行 → LineEndingLF（固定）；纯 LF → lf；纯 CRLF → crlf；
// CR-only 或 LF/CRLF 混合 → 返回 ok=false（映射 mixed_line_endings）。
func DetectLineEnding(b []byte) (le LineEnding, ok bool) {
	hasLF, hasCRLF, hasBareCR, hasLoneLF := false, false, false, false
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			hasLF = true
			if i > 0 && b[i-1] == '\r' {
				hasCRLF = true
			} else {
				hasLoneLF = true
			}
		} else if b[i] == '\r' {
			if i+1 >= len(b) || b[i+1] != '\n' {
				hasBareCR = true
			}
		}
	}
	switch {
	case !hasLF && !hasBareCR:
		return LineEndingLF, true // 无换行固定 lf
	case hasBareCR:
		return "", false // CR-only 或含裸 CR → mixed
	case hasCRLF && hasLoneLF:
		return "", false // CRLF 与孤立 LF 混合 → mixed
	case hasCRLF:
		return LineEndingCRLF, true
	case hasLF:
		return LineEndingLF, true
	default:
		return "", false
	}
}

// DeriveBOM 判定原始字节是否以 UTF-8 BOM 开头（design.md D5）。
func DeriveBOM(b []byte) bool {
	return len(b) >= 3 && b[0] == utf8BOM[0] && b[1] == utf8BOM[1] && b[2] == utf8BOM[2]
}

// StripBOM 去除 UTF-8 BOM 前缀（若存在）。
func StripBOM(b []byte) []byte {
	if DeriveBOM(b) {
		return b[3:]
	}
	return b
}

// NormalizeContentForRead 将原始字节（已去 BOM）的 CRLF 归一为 LF，供 editable=true 响应。
// 调用前应已确认换行风格为 lf 或 crlf（非 mixed）。
func NormalizeContentForRead(b []byte) string {
	// CRLF → LF。纯 LF 或无换行时 strings.ReplaceAll 无副作用。
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// RebuildWriteBytes 按请求携带的 lineEnding 重建写回字节（design.md D5 步骤 5）。
// content MUST 仅含 \n 换行（Service 已校验）。hasBOM 决定是否恢复 UTF-8 BOM 前缀。
// lineEnding=crlf → \n 替换为 \r\n；lf → 保持 \n。末尾换行状态以 content 表达为准。
func RebuildWriteBytes(content string, le LineEnding, hasBOM bool) []byte {
	var out []byte
	if hasBOM {
		out = append(out, utf8BOM...)
	}
	if le == LineEndingCRLF {
		out = append(out, strings.ReplaceAll(content, "\n", "\r\n")...)
	} else {
		out = append(out, content...)
	}
	return out
}

// SHA256Hex 返回 b 的 SHA-256 小写 hex（design.md D5 baseHash）。
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ModeToOctalString 将完整 chmod 值（uint32，含特殊位）格式化为四位八进制字符串。
// 输入应为 os.FileMode 的 uint32 表示（info.Mode()）。输出如 "0644"、"4755"。
func ModeToOctalString(mode uint32) string {
	return fmt.Sprintf("%04o", mode)
}

// ParseBaseMode 校验 baseMode 为四位八进制字符串 0000..7777（design.md D5 步骤 1）。
// 返回解析后的完整 chmod 值（uint32）与 ok。非法格式（非四位、非八进制）→ ok=false。
func ParseBaseMode(s string) (mode uint32, ok bool) {
	if len(s) != 4 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
	}
	var m uint32
	for _, c := range s {
		m = m*8 + uint32(c-'0')
	}
	return m, true
}

// HasOwnerWrite 判定 mode 是否有 owner 写位（design.md D5：mode & 0200 != 0）。
func HasOwnerWrite(mode uint32) bool {
	return mode&0o200 != 0
}

// IsValidBaseHash 校验 baseHash 为 64 位小写 hex（design.md D5 步骤 1）。
func IsValidBaseHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// --- 文件编辑用例（Service 方法，design.md D5） ---
//
// 文件编辑读写用例归属 Service（单一协调器，design.md D9）。FileEditPort 为可空端口：
// 部署不接线文件编辑能力时 fileEdit=nil，ReadFile/WriteFile 返回 ErrFileEditPortMissing。
// 领域逻辑（reasonCode 判定、BOM 推导、换行重建、SHA-256）由本包纯函数承载（见上），
// adapter 仅协调文件系统操作（见 internal/task/diffreview_fileedit.go）。

// ErrFileEditPortMissing 运行时未注入 FileEditPort（文件编辑能力未接线）。
// ReadFile/WriteFile 在 fileEdit=nil 时返回本错误（非构造期 panic——FileEditPort 为可空端口）。
var ErrFileEditPortMissing = errors.New("diffreview: file edit port not configured")

// ReadFile 实现 design.md D5 判别联合读取（tasks 3.9）。
// 经 FileEditPort.ReadRaw 获取常规文件原始字节 + 完整 mode，随后做 reasonCode 判定：
// missing/not_regular（adapter 经 FileEditReadRawError 指示）→ binary → too_large →
// non_utf8 → mixed_line_endings → read_only；全部通过 → editable=true 响应。
func (s *Service) ReadFile(ctx context.Context, taskID, path string) (FileEditReadResult, error) {
	if s.fileEdit == nil {
		return FileEditReadResult{}, ErrFileEditPortMissing
	}
	raw, err := s.fileEdit.ReadRaw(ctx, taskID, path)
	if err != nil {
		var re *FileEditReadRawError
		if errors.As(err, &re) {
			if re.NotRegular {
				return FileEditReadResult{Editable: false, ReasonCode: ReasonNotRegular, Reason: "not a regular file"}, nil
			}
			return FileEditReadResult{Editable: false, ReasonCode: ReasonMissing, Reason: "file does not exist"}, nil
		}
		return FileEditReadResult{}, err
	}
	if !raw.Exists {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonMissing, Reason: "file does not exist"}, nil
	}

	// 判定链（design.md D5 顺序，禁锢/regular 已由 adapter 完成）：
	b := raw.Bytes

	// NUL 二进制嗅探（前 8000 字节含 NUL → binary）。
	if IsBinaryBytes(b) {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonBinary, Reason: "binary file (NUL detected)"}, nil
	}
	// ≤512KiB（读取时取 FileEditMaxBytes+1，超过即 too_large）。
	if len(b) > FileEditMaxBytes {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonTooLarge, Reason: "file exceeds 512KiB"}, nil
	}
	// 有效 UTF-8。
	if !utf8.Valid(b) {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonNonUTF8, Reason: "invalid UTF-8"}, nil
	}
	// 换行风格统一 LF 或 CRLF（CR-only 或混合 → mixed_line_endings）。
	le, leOK := DetectLineEnding(b)
	if !leOK {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonMixedLineEndings, Reason: "mixed or CR-only line endings"}, nil
	}
	// owner 写位（mode & 0200 == 0 → read_only）。
	if !HasOwnerWrite(raw.Mode) {
		return FileEditReadResult{Editable: false, ReasonCode: ReasonReadOnly, Reason: "file is read-only (no owner write bit)"}, nil
	}

	// editable=true：组装响应。
	stripped := StripBOM(b)
	content := NormalizeContentForRead(stripped)
	return FileEditReadResult{
		Editable:   true,
		Content:    content,
		BaseHash:   SHA256Hex(b),
		LineEnding: le,
		HasBOM:     DeriveBOM(b),
		Mode:       ModeToOctalString(raw.Mode),
	}, nil
}

// WriteFile 实现 design.md D5 固定顺序原子写回（tasks 3.10）。
// 步骤 1（领域格式校验）在本方法完成；步骤 2-9 委托 FileEditPort.Write（adapter 持锁）。
func (s *Service) WriteFile(ctx context.Context, taskID string, req FileEditWriteRequest) (FileEditWriteResult, error) {
	if s.fileEdit == nil {
		return FileEditWriteResult{}, ErrFileEditPortMissing
	}
	// 步骤 1：领域格式校验（design.md D5 逐字）。
	// baseHash MUST 64 位小写 hex。
	if !IsValidBaseHash(req.BaseHash) {
		return FileEditWriteResult{}, newFileEditErr(ReasonInvalidInput, "baseHash must be 64-char lowercase hex")
	}
	// content MUST 仅以 \n 为换行，含任何 \r → invalid_input。
	if strings.ContainsRune(req.Content, '\r') {
		return FileEditWriteResult{}, newFileEditErr(ReasonInvalidInput, "content must use \\n line endings only (\\r forbidden)")
	}
	// lineEnding MUST ∈ lf|crlf。
	if req.LineEnding != LineEndingLF && req.LineEnding != LineEndingCRLF {
		return FileEditWriteResult{}, newFileEditErr(ReasonInvalidInput, "lineEnding must be \"lf\" or \"crlf\"")
	}
	// baseMode MUST 四位八进制 0000..7777。
	baseMode, ok := ParseBaseMode(req.BaseMode)
	if !ok {
		return FileEditWriteResult{}, newFileEditErr(ReasonInvalidInput, "baseMode must be 4-digit octal 0000..7777")
	}
	// baseMode 无 owner 写位 → invalid_input（只读文件禁止编辑）。
	if !HasOwnerWrite(baseMode) {
		return FileEditWriteResult{}, newFileEditErr(ReasonInvalidInput, "baseMode has no owner write bit (read-only file)")
	}

	// 步骤 2-9 委托 adapter（持锁、禁锢、重读比对、重建、临时文件、终检、rename）。
	return s.fileEdit.Write(ctx, taskID, req)
}

// --- 错误（domain 层 err-first，供 service 用例返回，adapter 映射为 code） ---

// FileEditErr 为文件编辑用例的领域错误（err-first，携带 ReasonCode 供 adapter 映射）。
// adapter 层据此映射 OpError code（invalid_input/conflict/invalid_state/internal）。
type FileEditErr struct {
	ReasonCode ReadReasonCode
	Reason     string
}

func (e *FileEditErr) Error() string {
	return fmt.Sprintf("diffreview: file edit %s: %s", e.ReasonCode, e.Reason)
}

// newFileEditErr 构造 FileEditErr。
func newFileEditErr(code ReadReasonCode, reason string) *FileEditErr {
	return &FileEditErr{ReasonCode: code, Reason: reason}
}

// 步骤 1 领域校验失败统一用 invalid_input ReasonCode（adapter 映射 application.CodeInvalidInput）。
const ReasonInvalidInput ReadReasonCode = "invalid_input"
