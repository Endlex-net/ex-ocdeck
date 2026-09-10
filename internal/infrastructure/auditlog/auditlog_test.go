package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mustReadLines 读回审计文件的非空行。
func mustReadLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// sampleEntry 字段完整的样例记录。
func sampleEntry() Entry {
	return Entry{
		Time:        "2026-09-10T12:00:00Z",
		TaskID:      "t1",
		TaskName:    "my task",
		RequestID:   "perm-1",
		Permission:  "bash",
		Patterns:    []string{"rm", "ls"},
		Verdict:     "APPROVE",
		ReplyResult: "ok",
		Reply:       "once",
	}
}

// TestLogger_FieldsCompleteJSONL 字段完整 JSONL 行：一行一条 JSON 对象，字段名与
// spec「ai-auto 权限判定审计日志」逐字对应；ok 记录含 reply 字段。
func TestLogger_FieldsCompleteJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	if err := l.Record(sampleEntry()); err != nil {
		t.Fatalf("Record: %v", err)
	}

	lines := mustReadLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (一行一条 JSON)", len(lines))
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	// 字段集合与值逐字校验（含 detail；字段必有，无详情时空串）。
	want := map[string]interface{}{
		"time": "2026-09-10T12:00:00Z", "task_id": "t1", "task_name": "my task",
		"request_id": "perm-1", "permission": "bash", "verdict": "APPROVE",
		"reply_result": "ok", "reply": "once", "detail": "",
	}
	for k, v := range want {
		got, ok := raw[k]
		if !ok {
			t.Errorf("field %q missing", k)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(v) {
			t.Errorf("field %q = %v, want %v", k, got, v)
		}
	}
	pats, ok := raw["patterns"].([]interface{})
	if !ok || len(pats) != 2 || pats[0] != "rm" || pats[1] != "ls" {
		t.Errorf("patterns = %v, want [rm ls]", raw["patterns"])
	}
}

// TestLogger_NotApplicable_NoReplyField not_applicable 记录不含 reply 字段
//（spec：仅 APPROVE/REJECT 且 ok 时额外包含 reply）。
func TestLogger_NotApplicable_NoReplyField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	e := sampleEntry()
	e.Verdict = "FAILED"
	e.ReplyResult = "not_applicable"
	e.Reply = ""
	if err := l.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(mustReadLines(t, path)[0]), &raw); err != nil {
		t.Fatalf("line not valid JSON: %v", err)
	}
	if _, present := raw["reply"]; present {
		t.Errorf("not_applicable record MUST NOT contain reply field, got %v", raw["reply"])
	}
	if raw["verdict"] != "FAILED" || raw["reply_result"] != "not_applicable" {
		t.Errorf("verdict/reply_result = %v/%v, want FAILED/not_applicable", raw["verdict"], raw["reply_result"])
	}
}

// TestNewPermAuditLogger_0600 新建文件权限 0600。
func TestNewPermAuditLogger_0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	defer l.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
}

// TestNewPermAuditLogger_Preset0644_TightenedKeepsContent 预置 0644 既有文件：
// 构造后收紧为 0600 且内容保留、新记录追加（runner.go 先例：OpenFile 创建权限
// 参数不修正既有文件，MUST 显式 Chmod）。
func TestNewPermAuditLogger_Preset0644_TightenedKeepsContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	// 预置 0644 既有文件，写入一条历史行。
	if err := os.WriteFile(path, []byte(`{"task_id":"old"}`+"\n"), 0o644); err != nil {
		t.Fatalf("preset file: %v", err)
	}

	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	defer l.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm after construction = %o, want 600 (既有文件 MUST 收紧)", fi.Mode().Perm())
	}
	if err := l.Record(sampleEntry()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	lines := mustReadLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2（既有内容保留 + 新记录追加）", len(lines))
	}
	var old map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &old); err != nil || old["task_id"] != "old" {
		t.Errorf("preset content must be preserved, got %s", lines[0])
	}
}

// TestLogger_Append_TwoRecordsTwoLines 追加语义：两次 Record 恰好两行（不截断）。
func TestLogger_Append_TwoRecordsTwoLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	if err := l.Record(sampleEntry()); err != nil {
		t.Fatalf("Record 1: %v", err)
	}
	e := sampleEntry()
	e.RequestID = "perm-2"
	e.Verdict = "REJECT"
	if err := l.Record(e); err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	if lines := mustReadLines(t, path); len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
}

// TestLogger_ConcurrentNoInterleave 并发不交错：N goroutine 各写 M 条，
// 读回 N*M 行且每行均为合法完整 JSON（一次 Write 落盘）。
func TestLogger_ConcurrentNoInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	const goroutines, perG = 8, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				e := sampleEntry()
				e.RequestID = fmt.Sprintf("p%d-%d", g, i)
				if err := l.Record(e); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	lines := mustReadLines(t, path)
	if len(lines) != goroutines*perG {
		t.Fatalf("lines = %d, want %d", len(lines), goroutines*perG)
	}
	for i, line := range lines {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("line %d corrupted (interleaved?): %v", i, err)
		}
		if raw["request_id"] == nil || raw["task_id"] != "t1" {
			t.Errorf("line %d fields broken: %s", i, line)
		}
	}
}

// TestLogger_Record_WriteErrorAfterClose 写入失败返回错误（句柄已关闭场景）。
func TestLogger_Record_WriteErrorAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai-permission-audit.jsonl")
	l, err := NewPermAuditLogger(path)
	if err != nil {
		t.Fatalf("NewPermAuditLogger: %v", err)
	}
	if err := l.Record(sampleEntry()); err != nil {
		t.Fatalf("Record before close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Record(sampleEntry()); err == nil {
		t.Fatal("Record after close MUST return error (调用方降级为普通日志)")
	}
}
