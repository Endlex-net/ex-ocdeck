package task

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExtractRequestDetail_Matrix 提取表驱动（tasks 8.2，严格按 D5 提取表）：
// 类别字段路径/类型/必填/完整性条件、missing/malformed/truncated 归一与优先级。
func TestExtractRequestDetail_Matrix(t *testing.T) {
	cases := []struct {
		name        string
		permission  string
		metadata    string // "" = 观察层归 nil；否则 JSON object 字面量
		wantDegraded string
		check       func(t *testing.T, d RequestDetail)
	}{
		{
			name: "bash command 完整", permission: "bash",
			metadata: `{"command":"rm -rf build"}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Command != "rm -rf build" || d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "bash command 缺失 → missing_critical", permission: "bash",
			metadata:   `{}`,
			wantDegraded: DegradedMissing,
		},
		{
			name: "bash metadata 缺失（观察层 nil）→ missing_critical", permission: "bash",
			metadata:   "",
			wantDegraded: DegradedMissing,
		},
		{
			name: "bash command 非 string → malformed_critical", permission: "bash",
			metadata:   `{"command":42}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "bash command 空串 → malformed_critical", permission: "bash",
			metadata:   `{"command":""}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "edit filepath+diff 完整", permission: "edit",
			metadata: `{"filepath":"a.go","diff":"-x\n+y"}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Filepath != "a.go" || d.Diff != "-x\n+y" || d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "edit diff 缺失 → missing_critical", permission: "write",
			metadata:   `{"filepath":"a.go"}`,
			wantDegraded: DegradedMissing,
		},
		{
			name: "F11 非法成员在前：长/短 patch 状态准确归属", permission: "apply_patch",
			// 非法 type 成员（跳过）→ 合法长 patch 成员（输出索引 0，字段级截断）
			// → 合法短 patch 成员（输出索引 1）。状态必须随成员身份归属：
			// 输出[0].diff 带标记、输出[1] 无标记。
			metadata: `{"files":[` +
				`{"type":"chmod","relativePath":"bad.go","patch":"p"},` +
				`{"type":"update","relativePath":"long.go","patch":"` + strings.Repeat("x", detailMaxDiff+10) + `"},` +
				`{"type":"update","relativePath":"short.go","patch":"tiny"}]}`,
			// 状态必须随成员身份归属：输出[0].diff 带标记、输出[1] 无标记。
			// Degraded 按优先级取 malformed_critical（非法成员 > patch 截断）。
			wantDegraded: DegradedMalformed,
			check: func(t *testing.T, d RequestDetail) {
				if len(d.Files) != 2 {
					t.Fatalf("files = %d, want 2", len(d.Files))
				}
				if d.Files[0].RelativePath != "long.go" || !strings.HasSuffix(d.Files[0].Patch, permTruncMarker) {
					t.Errorf("files[0] = %+v, want long.go with truncated patch", d.Files[0])
				}
				if d.Files[1].Patch != "tiny" || strings.Contains(d.Files[1].Patch, permTruncMarker) {
					t.Errorf("files[1] = %+v, want tiny patch without marker", d.Files[1])
				}
			},
		},
		{
			name: "apply_patch 成员完整", permission: "apply_patch",
			metadata: `{"files":[{"type":"update","relativePath":"a.go","patch":"@@ -1 +1 @@"},{"type":"move","relativePath":"b.go","movePath":"c.go"}]}`,
			check: func(t *testing.T, d RequestDetail) {
				if len(d.Files) != 2 || d.Files[1].MovePath != "c.go" || d.Files[1].Patch != "" || d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "apply_patch 空数组 → malformed_critical", permission: "apply_patch",
			metadata:   `{"files":[]}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "apply_patch 枚举外 type → malformed_critical", permission: "apply_patch",
			metadata:   `{"files":[{"type":"chmod","relativePath":"a.go","patch":"x"}]}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "apply_patch 成员必填 relativePath 缺失 → malformed_critical", permission: "apply_patch",
			metadata:   `{"files":[{"type":"update","patch":"x"}]}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "apply_patch move 缺 movePath → malformed_critical", permission: "apply_patch",
			metadata:   `{"files":[{"type":"move","relativePath":"b.go"}]}`,
			wantDegraded: DegradedMalformed,
		},
		{
			name: "webfetch url 可选（缺失以 patterns 判定）", permission: "webfetch",
			metadata: `{}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Degraded != "" {
					t.Errorf("optional-category missing metadata field MUST NOT degrade, got %+v", d)
				}
			},
		},
		{
			// F5：可选字段形状错误 → 舍弃该字段按 fallback 判定（patterns 已含 URL），
			// 不降级；有配置 Judge 仍收到 permission+patterns。
			name: "webfetch url 类型错 → 舍弃字段不降级", permission: "webfetch",
			metadata: `{"url":true}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Degraded != "" || d.URL != "" {
					t.Errorf("optional field type error MUST be dropped without degradation, got %+v", d)
				}
			},
		},
		{
			name: "read 无提取字段（patterns 即路径）", permission: "read",
			metadata: `{}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "未识别类别 → 仅 permission+patterns", permission: "unknown_tool",
			metadata: `{"anything":1}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Degraded != "" || d.Command != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "external_directory 两类来源并存都提取（F3）", permission: "external_directory",
			metadata: `{"command":"cat /outside/a","filepath":"/outside/a","parentDir":"/outside"}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Command != "cat /outside/a" || d.Filepath != "/outside/a" || d.ParentDir != "/outside" || d.Degraded != "" {
					t.Errorf("both sources MUST be extracted, got %+v", d)
				}
			},
		},
		{
			name: "external_directory 文件来源完整", permission: "external_directory",
			metadata: `{"filepath":"/outside/a.go","parentDir":"/outside"}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Filepath != "/outside/a.go" || d.ParentDir != "/outside" || d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "external_directory 全缺失 → missing_critical", permission: "external_directory",
			metadata:   `{}`,
			wantDegraded: DegradedMissing,
		},
		{
			name: "grep 可选字段携带", permission: "grep",
			metadata: `{"pattern":"foo","path":"/src","include":"*.go"}`,
			check: func(t *testing.T, d RequestDetail) {
				if d.Pattern != "foo" || d.Path != "/src" || d.Include != "*.go" || d.Degraded != "" {
					t.Errorf("got %+v", d)
				}
			},
		},
		{
			name: "metadata 合法 JSON 非 object → 观察层 nil → 关键类别 missing", permission: "bash",
			metadata:    `["not","object"]`,
			wantDegraded: DegradedMissing,
		},
		{
			name: "多原因优先级 malformed > missing > truncated", permission: "apply_patch",
			// 成员形状错误（malformed）与顶层 files 存在但含缺失（不会到 missing——构造：patch 截断 + 枚举外 type）
			metadata:   `{"files":[{"type":"chmod","relativePath":"a.go","patch":"` + strings.Repeat("x", 5000) + `"}]}`,
			wantDegraded: DegradedMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var meta json.RawMessage
			if tc.metadata != "" {
				meta = json.RawMessage(tc.metadata)
			}
			d := extractRequestDetail(tc.permission, meta)
			if d.Degraded != tc.wantDegraded {
				t.Errorf("Degraded = %q, want %q", d.Degraded, tc.wantDegraded)
			}
			if tc.check != nil {
				tc.check(t, d)
			}
		})
	}
}

// TestExtractRequestDetail_Truncation 截断语义（tasks 8.2）：字段级界值、UTF-8 安全、
// 标记不计入限额、files 数量裁剪、总量裁剪与关键证据判定。
func TestExtractRequestDetail_Truncation(t *testing.T) {
	t.Run("truncateUTF8 字节安全与标记不计入限额", func(t *testing.T) {
		// 多字节（中文 3B/字符）：max=10 → 保留 3 个字符（9B）+ 标记。
		s := strings.Repeat("中", 10) // 30B
		out, trunc := truncateUTF8(s, 10)
		if !trunc || len(out) != 9+len(permTruncMarker) {
			t.Fatalf("out len = %d, want %d; trunc=%v", len(out), 9+len(permTruncMarker), trunc)
		}
		// 保留部分必须是合法 UTF-8（无切断序列）。
		if !utf8.ValidString(out[:len(out)-len(permTruncMarker)]) {
			t.Errorf("truncated content is not valid UTF-8: %q", out)
		}
		full, trunc2 := truncateUTF8("short", 10)
		if trunc2 || full != "short" {
			t.Errorf("within-limit string must pass through, got %q", full)
		}
	})

	t.Run("command 恰好界值/超一字节", func(t *testing.T) {
		exact := strings.Repeat("a", detailMaxCommand)
		d := extractRequestDetail("bash", json.RawMessage(`{"command":"`+exact+`"}`))
		if d.Degraded != "" || len(d.Command) != detailMaxCommand {
			t.Errorf("exact boundary must not truncate: len=%d degraded=%q", len(d.Command), d.Degraded)
		}
		over := strings.Repeat("a", detailMaxCommand+1)
		d = extractRequestDetail("bash", json.RawMessage(`{"command":"`+over+`"}`))
		if d.Degraded != DegradedTruncated {
			t.Errorf("one-byte-over MUST set truncated_critical, got %q", d.Degraded)
		}
		if !strings.HasSuffix(d.Command, permTruncMarker) {
			t.Error("truncated command must end with marker")
		}
	})

	t.Run("files 数量裁剪 20/21 → truncated_critical", func(t *testing.T) {
		mk := func(n int) string {
			var sb strings.Builder
			sb.WriteString(`{"files":[`)
			for i := 0; i < n; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`{"type":"update","relativePath":"f` + strconv.Itoa(i) + `","patch":"p"}`)
			}
			sb.WriteString(`]}`)
			return sb.String()
		}
		d := extractRequestDetail("apply_patch", json.RawMessage(mk(detailMaxFiles)))
		if d.Degraded != "" || len(d.Files) != detailMaxFiles {
			t.Errorf("exactly 20 files: degraded=%q len=%d", d.Degraded, len(d.Files))
		}
		d21 := extractRequestDetail("apply_patch", json.RawMessage(mk(detailMaxFiles + 1)))
		if d21.Degraded != DegradedTruncated || len(d21.Files) != detailMaxFiles {
			t.Errorf("21 files: degraded=%q len=%d, want truncated + 20", d21.Degraded, len(d21.Files))
		}
		// 裁尾部：保留的是前 20 个。
		if d21.Files[19].RelativePath != "f"+strconv.Itoa(19) {
			t.Errorf("tail must be trimmed: kept last = %s", d21.Files[19].RelativePath)
		}
	})

	t.Run("edit diff 字段级截断（4000B 内不触发总量）→ truncated_critical", func(t *testing.T) {
		// diff 5000B 超 diff 字段级限额（4096）但 < 总量 8192：纯字段级截断。
		diff := strings.Repeat("x", 5000)
		meta := `{"filepath":"src/a.go","diff":"` + diff + `"}`
		d := extractRequestDetail("edit", json.RawMessage(meta))
		if d.Degraded != DegradedTruncated {
			t.Fatalf("degraded = %q, want truncated_critical", d.Degraded)
		}
		if d.Filepath != "src/a.go" {
			t.Errorf("earlier-order small field MUST be preserved intact, got %q", d.Filepath)
		}
		if len(d.Diff) != detailMaxDiff+len(permTruncMarker) {
			t.Errorf("field-level truncation: diff len = %d, want %d", len(d.Diff), detailMaxDiff+len(permTruncMarker))
		}
		if !strings.HasSuffix(d.Diff, permTruncMarker) {
			t.Error("diff must end with truncation marker")
		}
	})

	t.Run("真正只在总量阶段超限（20×500B patch，无字段级截断）", func(t *testing.T) {
		// 每 patch 500B ≤ 字段级 4096；内容总量 10190B > 总量 8192 → 纯总量裁剪。
		var sb strings.Builder
		sb.WriteString(`{"files":[`)
		for i := 0; i < 20; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`{"type":"update","relativePath":"f` + strconv.Itoa(i) + `","patch":"` + strings.Repeat("p", 500) + `"}`)
		}
		sb.WriteString(`]}`)
		d := extractRequestDetail("apply_patch", json.RawMessage(sb.String()))
		if d.Degraded != DegradedTruncated {
			t.Fatalf("degraded = %q, want truncated_critical (总量裁剪回写，F1)", d.Degraded)
		}
		// 精确保留结果（F8/oracle）：逆向缩减收敛后——
		//   成员 0..15：patch 完整 500B；成员 16：patch 内容恰 49B + 标记；
		//   成员 17..19：patch/relativePath/type 预算耗尽被裁（渲染为标记）。
		for i := 0; i < 16; i++ {
			if d.Files[i].Patch != strings.Repeat("p", 500) {
				t.Fatalf("member %d patch must be preserved intact, len=%d", i, len(d.Files[i].Patch))
			}
			if strings.HasSuffix(d.Files[i].Patch, permTruncMarker) {
				t.Fatalf("member %d patch must have no marker", i)
			}
			if d.Files[i].RelativePath != "f"+strconv.Itoa(i) {
				t.Errorf("member %d relativePath must be preserved, got %q", i, d.Files[i].RelativePath)
			}
		}
		want16 := strings.Repeat("p", 49) + permTruncMarker
		if d.Files[16].Patch != want16 {
			t.Fatalf("member 16 patch = len %d, want content 49B + marker", len(d.Files[16].Patch))
		}
		for i := 17; i < 20; i++ {
			if d.Files[i].Patch != permTruncMarker || d.Files[i].RelativePath != permTruncMarker {
				t.Errorf("member %d fields (budget exhausted) = %q/%q, want markers only",
					i, d.Files[i].Patch, d.Files[i].RelativePath)
			}
		}
		content := 0
		for _, f := range d.Files {
			content += len(strings.TrimSuffix(f.Type, permTruncMarker)) +
				len(strings.TrimSuffix(f.Patch, permTruncMarker)) +
				len(strings.TrimSuffix(f.RelativePath, permTruncMarker))
		}
		if content != detailMaxTotal {
			t.Errorf("total content = %d, want exactly %d (F2：标记不计入预算)", content, detailMaxTotal)
		}
	})

	t.Run("F2 巨大 relativePath 计入总量预算", func(t *testing.T) {
		// relativePath 9000B（无字段级限额）+ 小 patch：内容总 9004B > 8192 → 总量裁剪
		// 波及 relativePath（关键字段）→ truncated_critical。
		meta := `{"files":[{"type":"update","relativePath":"` + strings.Repeat("f", 9000) + `","patch":"p"}]}`
		d := extractRequestDetail("apply_patch", json.RawMessage(meta))
		if d.Degraded != DegradedTruncated {
			t.Fatalf("degraded = %q, want truncated_critical", d.Degraded)
		}
		// 标记不计入预算：内容（除标记）≤ 8192，输出 = 内容 + 标记。
		kept := d.Files[0].RelativePath
		if content := kept[:len(kept)-len(permTruncMarker)]; len(content) > detailMaxTotal {
			t.Errorf("relativePath content kept = %d bytes, want <= total budget %d", len(content), detailMaxTotal)
		}
		if !strings.HasSuffix(kept, permTruncMarker) {
			t.Error("truncated relativePath must end with marker")
		}
	})

	t.Run("F2 巨大 directories 逐元素计入总量预算", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString(`{"command":"cat x","directories":[`)
		for i := 0; i < 10; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(`"/dir/` + strings.Repeat("d", 1000) + `"`)
		}
		sb.WriteString(`]}`)
		d := extractRequestDetail("external_directory", json.RawMessage(sb.String()))
		if d.Degraded != DegradedTruncated {
			t.Fatalf("degraded = %q, want truncated_critical（directories 波及关键目标）", d.Degraded)
		}
		// 逆向：末尾元素被裁，前部保留。
		if d.Directories[0] != "/dir/"+strings.Repeat("d", 1000) {
			t.Errorf("front elements must be preserved, got %q", d.Directories[0])
		}
	})

	t.Run("多原因优先级 malformed > truncated", func(t *testing.T) {
		// 成员形状错误（malformed）+ 合法成员 patch 字段级截断（truncated）
		// → 按固定优先级取 malformed。
		long := strings.Repeat("x", detailMaxDiff+10)
		meta := `{"files":[{"type":"chmod","relativePath":"bad.go","patch":"p"},{"type":"update","relativePath":"ok.go","patch":"` + long + `"}]}`
		d := extractRequestDetail("apply_patch", json.RawMessage(meta))
		if d.Degraded != DegradedMalformed {
			t.Errorf("multi-cause priority: degraded = %q, want malformed_critical", d.Degraded)
		}
	})

	t.Run("F2 字段级截断后内容总量恰好 8192 → 总量阶段零裁剪", func(t *testing.T) {
		// filepath 4096B（无字段级限额）+ diff 4097B（字段级截到 4096B）：
		// 内容总量恰好 8192B → 总量阶段零裁剪；标记渲染时追加、不计入预算。
		meta := `{"filepath":"` + strings.Repeat("f", 4096) + `","diff":"` + strings.Repeat("d", 4097) + `"}`
		d := extractRequestDetail("edit", json.RawMessage(meta))
		if d.Degraded != DegradedTruncated {
			t.Fatalf("degraded = %q, want truncated_critical（仅字段级）", d.Degraded)
		}
		if d.Filepath != strings.Repeat("f", 4096) {
			t.Errorf("filepath must be untouched (无字段级限额), len=%d", len(d.Filepath))
		}
		if d.Diff != strings.Repeat("d", detailMaxDiff)+permTruncMarker {
			t.Errorf("diff = content(4096)+marker, got len=%d", len(d.Diff))
		}
		// 内容总量恰好 8192（标记豁免）。
		if got := len(d.Filepath) + len(d.Diff) - len(permTruncMarker); got != detailMaxTotal {
			t.Errorf("content total = %d, want exactly %d", got, detailMaxTotal)
		}
	})

	t.Run("F3 directories 成员形状矩阵", func(t *testing.T) {
		cases := []struct {
			name          string
			dirs          string // files 之外仅 directories 字段
			wantDegraded  string
			wantHasTarget bool
		}{
			{name: "空数组 → 无非空目标 missing", dirs: `{"directories":[]}`, wantDegraded: DegradedMissing},
			{name: "仅空串元素 → missing", dirs: `{"directories":[""]}`, wantDegraded: DegradedMissing},
			{name: "null 元素 → malformed（R1-F1）", dirs: `{"directories":[null]}`, wantDegraded: DegradedMalformed},
			{name: "合法与 null 并存 → malformed 且保留合法（R1-F1）", dirs: `{"directories":["/outside",null]}`, wantDegraded: DegradedMalformed,
				wantHasTarget: true},
			{name: "错误类型成员 → malformed", dirs: `{"directories":[42]}`, wantDegraded: DegradedMalformed},
			{name: "非数组 → malformed", dirs: `{"directories":"x"}`, wantDegraded: DegradedMalformed},
			{name: "合法与异常并存 → malformed 且保留合法", dirs: `{"directories":["/ok",42]}`, wantDegraded: DegradedMalformed,
				wantHasTarget: true},
			{name: "合法非空 → 无降级", dirs: `{"directories":["/d"]}`, wantHasTarget: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				d := extractRequestDetail("external_directory", json.RawMessage(tc.dirs))
				if d.Degraded != tc.wantDegraded {
					t.Errorf("Degraded = %q, want %q", d.Degraded, tc.wantDegraded)
				}
				if has := len(d.Directories) > 0 && d.Directories[0] != ""; has != tc.wantHasTarget {
					t.Errorf("hasTarget = %v (dirs=%v), want %v", has, d.Directories, tc.wantHasTarget)
				}
			})
		}
	})

	t.Run("F9 command 存在但形状错误 → malformed（filepath 保留完整性）", func(t *testing.T) {
		d := extractRequestDetail("external_directory", json.RawMessage(`{"command":42,"filepath":"/outside/a"}`))
		if d.Degraded != DegradedMalformed {
			t.Errorf("Degraded = %q, want malformed_critical（来源存在且形状错误）", d.Degraded)
		}
		if d.Filepath != "/outside/a" {
			t.Errorf("filepath = %q, want preserved", d.Filepath)
		}
	})

	t.Run("F9 command 2049B → 2048B 截断 + truncated_critical", func(t *testing.T) {
		d := extractRequestDetail("external_directory", json.RawMessage(`{"command":"` + strings.Repeat("c", 2049) + `","filepath":"/outside/a"}`))
		if d.Degraded != DegradedTruncated {
			t.Errorf("Degraded = %q, want truncated_critical", d.Degraded)
		}
		if content := strings.TrimSuffix(d.Command, permTruncMarker); len(content) != detailMaxCommand {
			t.Errorf("command content len = %d, want 2048（截断）", len(content))
		}
		if !strings.HasSuffix(d.Command, permTruncMarker) {
			t.Error("command must end with truncation marker")
		}
	})
}

// TestSummarizeRequestDetail 审计 detail 摘要（D11）：前缀不可截断、≤1024B 含标记、
// 空详情为空串。
func TestSummarizeRequestDetail(t *testing.T) {
	t.Run("空详情 → 空串", func(t *testing.T) {
		if got := summarizeRequestDetail(RequestDetail{}); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("降级前缀 + 主体", func(t *testing.T) {
		d := RequestDetail{Degraded: DegradedMissing, Command: ""}
		got := summarizeRequestDetail(d)
		if !strings.HasPrefix(got, DegradedMissing+": ") {
			t.Errorf("got %q, want prefix %q", got, DegradedMissing+": ")
		}
	})
	t.Run("超长主体截断且前缀保留", func(t *testing.T) {
		d := RequestDetail{
			Degraded: DegradedTruncated,
			Command:  strings.Repeat("中", auditDetailMax), // 3KB 多字节主体
		}
		got := summarizeRequestDetail(d)
		if len(got) > auditDetailMax {
			t.Errorf("detail len = %d, want <= %d", len(got), auditDetailMax)
		}
		if !strings.HasPrefix(got, DegradedTruncated+": ") {
			t.Errorf("prefix MUST survive truncation, got %q", got[:30])
		}
		if !strings.HasSuffix(got, permTruncMarker) {
			t.Error("body must end with truncation marker")
		}
	})
	t.Run("无降级超长主体也截断到 1024", func(t *testing.T) {
		d := RequestDetail{Command: strings.Repeat("a", 3000)}
		got := summarizeRequestDetail(d)
		if len(got) > auditDetailMax {
			t.Errorf("len = %d", len(got))
		}
	})
}

// TestNormalizePermissionMetadataForTest 观察层归一化（opencode 层函数经包内导出的
// 等价语义在 task 侧验证——nil/非 object 一律按缺失处理，见 detail_extract.go 入口）。
func TestExtractRequestDetail_NilAndNonObject(t *testing.T) {
	cases := []struct {
		name           string
		permission     string
		meta           json.RawMessage // nil 表示观察层归 nil
		wantDegraded   string
	}{
		{name: "nil + bash → missing", permission: "bash", wantDegraded: DegradedMissing},
		{name: "nil + read → 无降级", permission: "read"},
		{name: "非 object（观察层应归 nil，此处防御直入）+ bash → missing", permission: "bash", meta: json.RawMessage(`"str"`), wantDegraded: DegradedMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := extractRequestDetail(tc.permission, tc.meta)
			if d.Degraded != tc.wantDegraded {
				t.Errorf("Degraded = %q, want %q", d.Degraded, tc.wantDegraded)
			}
		})
	}
}

