package diffreview

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// --- 领域纯函数测试（design.md D5） ---

func TestIsBinaryBytes(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want bool
	}{
		{"plain text", []byte("hello\nworld\n"), false},
		{"empty", []byte{}, false},
		{"NUL in first 8000", []byte{'a', 0, 'b'}, true},
		{"NUL after 8000", append(bytes.Repeat([]byte{'a'}, 8001), 0), false}, // 超出嗅探窗口
	}
	for _, c := range cases {
		if got := IsBinaryBytes(c.b); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestDetectLineEnding(t *testing.T) {
	cases := []struct {
		name   string
		b      []byte
		wantLE LineEnding
		wantOK bool
	}{
		{"no newline", []byte("hello"), LineEndingLF, true},
		{"pure LF", []byte("a\nb\n"), LineEndingLF, true},
		{"pure CRLF", []byte("a\r\nb\r\n"), LineEndingCRLF, true},
		{"CR-only", []byte("a\rb\r"), "", false},
		{"LF+CRLF mixed", []byte("a\nb\r\nc\n"), "", false},
		{"CRLF+LF mixed", []byte("a\r\nb\nc\r\n"), "", false},
		{"empty", []byte(""), LineEndingLF, true},
		{"trailing CR only", []byte("a\nb\r"), "", false},
	}
	for _, c := range cases {
		gotLE, gotOK := DetectLineEnding(c.b)
		if gotLE != c.wantLE || gotOK != c.wantOK {
			t.Fatalf("%s: got (%q,%v) want (%q,%v)", c.name, gotLE, gotOK, c.wantLE, c.wantOK)
		}
	}
}

func TestDeriveBOM(t *testing.T) {
	if !DeriveBOM([]byte{0xEF, 0xBB, 0xBF, 'a'}) {
		t.Fatal("BOM present should be true")
	}
	if DeriveBOM([]byte{'a', 0xEF, 0xBB, 0xBF}) {
		t.Fatal("BOM not at start should be false")
	}
	if DeriveBOM([]byte("hello")) {
		t.Fatal("no BOM should be false")
	}
	if DeriveBOM([]byte{0xEF, 0xBB}) {
		t.Fatal("too short should be false")
	}
}

func TestStripBOM(t *testing.T) {
	stripped := StripBOM([]byte{0xEF, 0xBB, 0xBF, 'h', 'i'})
	if string(stripped) != "hi" {
		t.Fatalf("strip BOM: %q", stripped)
	}
	noBOM := StripBOM([]byte("hi"))
	if string(noBOM) != "hi" {
		t.Fatalf("no BOM strip: %q", noBOM)
	}
}

func TestNormalizeContentForRead(t *testing.T) {
	got := NormalizeContentForRead([]byte("a\r\nb\r\nc"))
	if got != "a\nb\nc" {
		t.Fatalf("CRLF normalize: %q", got)
	}
	// 纯 LF 无副作用
	if got := NormalizeContentForRead([]byte("a\nb\n")); got != "a\nb\n" {
		t.Fatalf("LF normalize: %q", got)
	}
}

func TestRebuildWriteBytes(t *testing.T) {
	// LF: 保持 \n
	got := RebuildWriteBytes("a\nb\n", LineEndingLF, false)
	if string(got) != "a\nb\n" {
		t.Fatalf("lf rebuild: %q", got)
	}
	// CRLF: \n → \r\n
	got = RebuildWriteBytes("a\nb\n", LineEndingCRLF, false)
	if string(got) != "a\r\nb\r\n" {
		t.Fatalf("crlf rebuild: %q", got)
	}
	// CRLF + BOM
	got = RebuildWriteBytes("a\n", LineEndingCRLF, true)
	want := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n'}
	if string(got) != string(want) {
		t.Fatalf("crlf+bom rebuild: %q", got)
	}
	// LF + BOM，末尾无换行保持
	got = RebuildWriteBytes("hello", LineEndingLF, true)
	want = []byte{0xEF, 0xBB, 0xBF, 'h', 'e', 'l', 'l', 'o'}
	if string(got) != string(want) {
		t.Fatalf("lf+bom no-newline rebuild: %q", got)
	}
}

func TestSHA256Hex(t *testing.T) {
	// SHA-256 of empty string
	got := SHA256Hex([]byte{})
	if got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("sha256 empty: %s", got)
	}
	// 小写 hex
	got = SHA256Hex([]byte("hello\n"))
	if got != "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" {
		t.Fatalf("sha256 hello: %s", got)
	}
}

func TestModeToOctalString(t *testing.T) {
	cases := []struct {
		mode uint32
		want string
	}{
		{0o644, "0644"},
		{0o755, "0755"},
		{0o4755, "4755"}, // setuid + 755
		{0o2755, "2755"}, // setgid + 755
		{0o1755, "1755"}, // sticky + 755
		{0o000, "0000"},
		{0o7777, "7777"},
	}
	for _, c := range cases {
		if got := ModeToOctalString(c.mode); got != c.want {
			t.Fatalf("mode %o: got %s want %s", c.mode, got, c.want)
		}
	}
}

func TestParseBaseMode(t *testing.T) {
	cases := []struct {
		s    string
		ok   bool
		mode uint32
	}{
		{"0644", true, 0o644},
		{"4755", true, 0o4755},
		{"7777", true, 0o7777},
		{"0000", true, 0o000},
		{"644", false, 0},   // 非四位
		{"06444", false, 0}, // 非四位
		{"0844", false, 0},  // 非八进制
		{"0x44", false, 0},  // 非八进制
		{"abcd", false, 0},  // 非八进制
		{"", false, 0},
	}
	for _, c := range cases {
		mode, ok := ParseBaseMode(c.s)
		if ok != c.ok || (ok && mode != c.mode) {
			t.Fatalf("ParseBaseMode(%q): got (%o,%v) want (%o,%v)", c.s, mode, ok, c.mode, c.ok)
		}
	}
}

func TestHasOwnerWrite(t *testing.T) {
	if !HasOwnerWrite(0o644) {
		t.Fatal("0644 should have owner write")
	}
	if HasOwnerWrite(0o444) {
		t.Fatal("0444 should not have owner write")
	}
	if HasOwnerWrite(0o755) {
		// 0755 owner=rwx → has write
	}
	if !HasOwnerWrite(0o755) {
		t.Fatal("0755 should have owner write")
	}
}

func TestIsValidBaseHash(t *testing.T) {
	if !IsValidBaseHash("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Fatal("valid 64-char lowercase hex should pass")
	}
	if IsValidBaseHash("E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855") {
		t.Fatal("uppercase hex should fail")
	}
	if IsValidBaseHash("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8") {
		t.Fatal("63-char should fail")
	}
	if IsValidBaseHash("z3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
		t.Fatal("non-hex char should fail")
	}
}

// --- FileEditService 测试（通过 fakeFileEditPort 测领域判定逻辑） ---

// fakeFileEditPort 为测试用 FileEditPort 实现，由测试直接控制返回值。
type fakeFileEditPort struct {
	rawResult   FileEditRawFile
	rawErr      error
	writeErr    error
	writeReq    FileEditWriteRequest
	writeResult FileEditWriteResult
}

func (f *fakeFileEditPort) ReadRaw(ctx context.Context, taskID, path string) (FileEditRawFile, error) {
	return f.rawResult, f.rawErr
}

func (f *fakeFileEditPort) Write(ctx context.Context, taskID string, req FileEditWriteRequest) (FileEditWriteResult, error) {
	f.writeReq = req
	return f.writeResult, f.writeErr
}

func TestFileEditService_ReadFile_ReasonCodes(t *testing.T) {
	cases := []struct {
		name     string
		raw      FileEditRawFile
		rawErr   error
		wantCode ReadReasonCode
		editable bool
	}{
		{
			name:     "missing (Exists=false)",
			raw:      FileEditRawFile{Exists: false},
			wantCode: ReasonMissing,
		},
		{
			name:     "not_regular (FileEditReadRawError NotRegular)",
			rawErr:   &FileEditReadRawError{NotRegular: true},
			wantCode: ReasonNotRegular,
		},
		{
			name:     "missing (FileEditReadRawError not NotRegular)",
			rawErr:   &FileEditReadRawError{NotRegular: false},
			wantCode: ReasonMissing,
		},
		{
			name:     "binary (NUL in first 8000)",
			raw:      FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte("hello\x00world\n")},
			wantCode: ReasonBinary,
		},
		{
			name:     "too_large (>512KiB)",
			raw:      FileEditRawFile{Exists: true, Mode: 0o644, Bytes: bytes.Repeat([]byte{'a'}, FileEditMaxBytes+1)},
			wantCode: ReasonTooLarge,
		},
		{
			name:     "non_utf8",
			raw:      FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte{0xFF, 0xFE, 'a'}},
			wantCode: ReasonNonUTF8,
		},
		{
			name:     "mixed_line_endings (LF+CRLF)",
			raw:      FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte("a\nb\r\nc\n")},
			wantCode: ReasonMixedLineEndings,
		},
		{
			name:     "mixed_line_endings (CR-only)",
			raw:      FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte("a\rb\r")},
			wantCode: ReasonMixedLineEndings,
		},
		{
			name:     "read_only (mode 0444)",
			raw:      FileEditRawFile{Exists: true, Mode: 0o444, Bytes: []byte("hello\n")},
			wantCode: ReasonReadOnly,
		},
	}
	for _, c := range cases {
		port := &fakeFileEditPort{rawResult: c.raw, rawErr: c.rawErr}
		svc := New(Options{FileEdit: port})
		res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", c.name, err)
		}
		if res.Editable != c.editable {
			t.Fatalf("%s: editable=%v want %v", c.name, res.Editable, c.editable)
		}
		if res.ReasonCode != c.wantCode {
			t.Fatalf("%s: reasonCode=%q want %q", c.name, res.ReasonCode, c.wantCode)
		}
	}
}

func TestFileEditService_ReadFile_Editable(t *testing.T) {
	// LF 文件，无 BOM
	raw := FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte("hello\nworld\n")}
	svc := New(Options{FileEdit: &fakeFileEditPort{rawResult: raw}})
	res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.Content != "hello\nworld\n" {
		t.Fatalf("content: %q", res.Content)
	}
	if res.LineEnding != LineEndingLF {
		t.Fatalf("lineEnding: %v", res.LineEnding)
	}
	if res.HasBOM {
		t.Fatal("hasBOM should be false")
	}
	if res.Mode != "0644" {
		t.Fatalf("mode: %q", res.Mode)
	}
	wantHash := SHA256Hex(raw.Bytes)
	if res.BaseHash != wantHash {
		t.Fatalf("baseHash: %q want %q", res.BaseHash, wantHash)
	}
}

func TestFileEditService_ReadFile_EditableCRLFWithBOM(t *testing.T) {
	// CRLF 文件 + BOM
	rawBytes := []byte{0xEF, 0xBB, 0xBF, 'a', '\r', '\n', 'b', '\r', '\n'}
	raw := FileEditRawFile{Exists: true, Mode: 0o755, Bytes: rawBytes}
	svc := New(Options{FileEdit: &fakeFileEditPort{rawResult: raw}})
	res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	// content 去BOM + CRLF→LF
	if res.Content != "a\nb\n" {
		t.Fatalf("content: %q", res.Content)
	}
	if res.LineEnding != LineEndingCRLF {
		t.Fatalf("lineEnding: %v", res.LineEnding)
	}
	if !res.HasBOM {
		t.Fatal("hasBOM should be true")
	}
	if res.Mode != "0755" {
		t.Fatalf("mode: %q", res.Mode)
	}
	// baseHash 是原始精确字节（含BOM+CRLF）
	if res.BaseHash != SHA256Hex(rawBytes) {
		t.Fatalf("baseHash mismatch")
	}
}

func TestFileEditService_ReadFile_EdibleNoNewline(t *testing.T) {
	// 无换行 → lineEnding 固定 lf
	raw := FileEditRawFile{Exists: true, Mode: 0o644, Bytes: []byte("hello")}
	svc := New(Options{FileEdit: &fakeFileEditPort{rawResult: raw}})
	res, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Editable {
		t.Fatalf("editable: %+v", res)
	}
	if res.LineEnding != LineEndingLF {
		t.Fatalf("lineEnding (no newline): %v want lf", res.LineEnding)
	}
	if res.Content != "hello" {
		t.Fatalf("content: %q", res.Content)
	}
}

func TestFileEditService_ReadFile_PortMissing(t *testing.T) {
	svc := New(Options{})
	_, err := svc.ReadFile(context.Background(), "t1", "f.txt")
	if !errors.Is(err, ErrFileEditPortMissing) {
		t.Fatalf("err: %v want ErrFileEditPortMissing", err)
	}
}

// --- WriteFile 步骤 1 领域校验测试 ---

func TestFileEditService_WriteFile_ValidationErrors(t *testing.T) {
	port := &fakeFileEditPort{writeResult: FileEditWriteResult{BaseHash: "x"}}
	cases := []struct {
		name string
		req  FileEditWriteRequest
	}{
		{
			name: "invalid baseHash (not 64 hex)",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: "short", LineEnding: LineEndingLF, BaseMode: "0644"},
		},
		{
			name: "invalid baseHash (uppercase)",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("A", 64), LineEnding: LineEndingLF, BaseMode: "0644"},
		},
		{
			name: "content contains CR",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\r\n", BaseHash: strings.Repeat("a", 64), LineEnding: LineEndingLF, BaseMode: "0644"},
		},
		{
			name: "content contains bare CR",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\rb", BaseHash: strings.Repeat("a", 64), LineEnding: LineEndingLF, BaseMode: "0644"},
		},
		{
			name: "invalid lineEnding",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64), LineEnding: "cr", BaseMode: "0644"},
		},
		{
			name: "invalid baseMode (not 4 digit)",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64), LineEnding: LineEndingLF, BaseMode: "644"},
		},
		{
			name: "invalid baseMode (non-octal)",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64), LineEnding: LineEndingLF, BaseMode: "0844"},
		},
		{
			name: "baseMode no owner write (read-only)",
			req:  FileEditWriteRequest{Path: "f.txt", Content: "a\n", BaseHash: strings.Repeat("a", 64), LineEnding: LineEndingLF, BaseMode: "0444"},
		},
	}
	for _, c := range cases {
		svc := New(Options{FileEdit: port})
		_, err := svc.WriteFile(context.Background(), "t1", c.req)
		if err == nil {
			t.Fatalf("%s: expected error, got nil", c.name)
		}
		var fe *FileEditErr
		if !errors.As(err, &fe) {
			t.Fatalf("%s: err type %T want *FileEditErr", c.name, err)
		}
		if fe.ReasonCode != ReasonInvalidInput {
			t.Fatalf("%s: reasonCode %q want invalid_input", c.name, fe.ReasonCode)
		}
		// 步骤 1 失败 MUST NOT 调用 port.Write（零写盘）
		if port.writeReq.Path != "" {
			t.Fatalf("%s: port.Write was called (should not reach adapter)", c.name)
		}
	}
}

func TestFileEditService_WriteFile_Valid_DelegatesToPort(t *testing.T) {
	port := &fakeFileEditPort{writeResult: FileEditWriteResult{BaseHash: strings.Repeat("b", 64)}}
	svc := New(Options{FileEdit: port})
	req := FileEditWriteRequest{
		Path:       "f.txt",
		Content:    "hello\n",
		BaseHash:   strings.Repeat("a", 64),
		LineEnding: LineEndingLF,
		BaseMode:   "0644",
	}
	res, err := svc.WriteFile(context.Background(), "t1", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseHash != strings.Repeat("b", 64) {
		t.Fatalf("baseHash: %q", res.BaseHash)
	}
	if port.writeReq.Path != "f.txt" {
		t.Fatalf("port.Write not called with correct req: %+v", port.writeReq)
	}
}

func TestFileEditService_WriteFile_PortMissing(t *testing.T) {
	svc := New(Options{})
	req := FileEditWriteRequest{
		Path:       "f.txt",
		Content:    "a\n",
		BaseHash:   strings.Repeat("a", 64),
		LineEnding: LineEndingLF,
		BaseMode:   "0644",
	}
	_, err := svc.WriteFile(context.Background(), "t1", req)
	if !errors.Is(err, ErrFileEditPortMissing) {
		t.Fatalf("err: %v want ErrFileEditPortMissing", err)
	}
}

func TestFileEditReadRawError(t *testing.T) {
	e := &FileEditReadRawError{NotRegular: true}
	if !errors.Is(e, e) {
		t.Fatal("should be itself")
	}
	e2 := &FileEditReadRawError{NotRegular: false}
	if e2.Error() != "diffreview: file missing" {
		t.Fatalf("missing error: %q", e2.Error())
	}
	if e.Error() != "diffreview: not a regular file" {
		t.Fatalf("not_regular error: %q", e.Error())
	}
}
