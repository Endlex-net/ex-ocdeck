package git

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// parseStatusPorcelainV2Z 流式解析 `git status --porcelain=v2 -z` 的输出。
// reader 中的条目以 NUL 分隔。当累计目标条目数超过 maxFiles 时返回 ErrTooManyFilesChanged。
// includeIgnored=false 时跳过 '!' ignored 条目（既有 Status 调用方默认不含 ignored）。
// kindFilter 为非 nil 时，仅首字节（kind）命中过滤器的记录才被解析/计数——非目标 kind
// 在解析阶段即跳过（不分配 FileStatus、不计数），避免大量非目标条目（如 modified tracked）
// 触发上限或造成无谓分配（design.md §7.2：ListIgnoredUntracked 仅需 ?/! 目标条目）。
func parseStatusPorcelainV2Z(reader io.Reader, maxFiles int, includeIgnored bool, kindFilter func(byte) bool) ([]FileStatus, error) {
	br := bufio.NewReader(reader)
	var result []FileStatus
	// 逐条读取 NUL 终止的记录。
	for {
		record, err := readNullTerminated(br)
		if err == io.EOF {
			if record == nil {
				break
			}
		} else if err != nil {
			return nil, fmt.Errorf("read porcelain v2: %w", err)
		}
		if len(record) == 0 {
			// 连续 NUL 或末尾：跳过。
			if err == io.EOF {
				break
			}
			continue
		}

		// kind 过滤：非目标 kind 在解析阶段即跳过（不分配、不计数）。
		// '2' rename 条目需额外读旧路径 NUL 记录——过滤时仍需跳过该记录以保持流位置。
		kind := record[0]
		if kindFilter != nil && !kindFilter(kind) {
			if kind == '2' {
				// rename 有第二条 NUL 记录（旧路径），必须消费以保持流位置。
				if _, nerr := readNullTerminated(br); nerr != nil && nerr != io.EOF {
					return nil, fmt.Errorf("read porcelain v2 rename path (filtered): %w", nerr)
				}
			}
			if err == io.EOF {
				break
			}
			continue
		}

		entry, perr := parsePorcelainEntry(record)
		if perr != nil {
			// 未能识别的条目类型：跳过该条记录。
			if err == io.EOF {
				break
			}
			continue
		}
		// ignored 条目：仅 includeIgnored=true 时保留（design.md §7.2：既有 Status 调用方
		// 默认不含 ignored；ListIgnoredUntracked 单独入口）。
		if entry != nil && entry.Kind == "!" && !includeIgnored {
			if err == io.EOF {
				break
			}
			continue
		}
		// rename 条目需继续读取下一条 NUL 记录作为旧路径（源）。
		if entry != nil && entry.Kind == "2" {
			next, nerr := readNullTerminated(br)
			if nerr != nil && nerr != io.EOF {
				return nil, fmt.Errorf("read porcelain v2 rename path: %w", nerr)
			}
			if len(next) == 0 {
				return nil, errors.New("porcelain v2: rename entry missing old path")
			}
			entry.Rename = string(next)
		}
		// 跳过目录占位条目（路径以 '/' 结尾）。
		if entry != nil {
			if len(entry.Path) > 0 && entry.Path[len(entry.Path)-1] == '/' {
				// 目录占位（如 untracked dir），不作为文件条目。
				if err == io.EOF {
					break
				}
				continue
			}
			result = append(result, *entry)
		}

		if len(result) > maxFiles {
			return nil, ErrTooManyFilesChanged
		}

		if err == io.EOF {
			break
		}
	}
	return result, nil
}

// readNullTermitted 读取直到 NUL 或 EOF；返回 record（不含 NUL）。
// 当遇到 EOF 且未读到任何字节时返回 (nil, io.EOF)；读到部分字节后 EOF 返回 (data, io.EOF)。
func readNullTerminated(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF {
				if buf.Len() == 0 {
					return nil, io.EOF
				}
				return buf.Bytes(), io.EOF
			}
			return buf.Bytes(), err
		}
		if b == 0 {
			return buf.Bytes(), nil
		}
		buf.WriteByte(b)
	}
}

// parsePorcelainEntry 解析单条 NUL 记录的前导部分（不含路径尾部的 NUL）。
// 返回 entry 与 error；对未识别类型返回 error（调用方跳过）。
func parsePorcelainEntry(record []byte) (*FileStatus, error) {
	if len(record) == 0 {
		return nil, errors.New("empty record")
	}
	kind := record[0]
	switch kind {
	case '1':
		return parseOrdinary(record)
	case '2':
		return parseRename(record)
	case 'u':
		return parseUnmerged(record)
	case '?':
		return parseUntracked(record)
	case '!':
		// ignored 条目（--ignored=traditional）：解析路径。
		return parseIgnored(record)
	default:
		return nil, fmt.Errorf("unknown porcelain v2 entry kind %q", kind)
	}
}

// ordinary: "1 XY sub mH mI mW hO iO path"
func parseOrdinary(record []byte) (*FileStatus, error) {
	fields, ok := splitPrefixFields(record, 8)
	if !ok {
		return nil, errors.New("malformed ordinary entry")
	}
	xy := string(fields[1])
	x, y := normalizeXY(xy)
	return &FileStatus{
		Kind:     "1",
		Path:     string(fields[8]),
		X:        x,
		Y:        y,
		Staged:   x != ' ' && x != '?',
		Unstaged: y != ' ' && y != '?',
	}, nil
}

// rename: "2 XY sub mH mI mW hO iO score newPath" + NUL + oldPath
// porcelain v2 中记录尾字段为新路径，第二条 NUL 记录为旧路径（源）。
func parseRename(record []byte) (*FileStatus, error) {
	fields, ok := splitPrefixFields(record, 9)
	if !ok {
		return nil, errors.New("malformed rename entry")
	}
	xy := string(fields[1])
	x, y := normalizeXY(xy)
	return &FileStatus{
		Kind:     "2",
		Path:     string(fields[9]), // 新路径（目标）
		Rename:   "",                // 旧路径由调用方填充
		X:        x,
		Y:        y,
		Staged:   x != ' ' && x != '?',
		Unstaged: y != ' ' && y != '?',
	}, nil
}

// unmerged: "u XY sub m1 m2 m3 mW h1 h2 h3 path"
func parseUnmerged(record []byte) (*FileStatus, error) {
	fields, ok := splitPrefixFields(record, 10)
	if !ok {
		return nil, errors.New("malformed unmerged entry")
	}
	xy := string(fields[1])
	x, y := normalizeXY(xy)
	return &FileStatus{
		Kind:     "u",
		Path:     string(fields[10]),
		X:        x,
		Y:        y,
		Staged:   x != ' ' && x != '?',
		Unstaged: y != ' ' && y != '?',
	}, nil
}

// untracked: "? path"
func parseUntracked(record []byte) (*FileStatus, error) {
	s := string(record)
	// 去掉 "? " 前缀。
	path := strings.TrimPrefix(s, "? ")
	if path == s {
		// 无前缀空格的异常，直接使用整条。
		path = s
	}
	return &FileStatus{
		Kind:      "?",
		Path:      path,
		X:         '?',
		Y:         '?',
		Untracked: true,
	}, nil
}

// ignored: "! path"（--ignored=traditional 的文件级记录，design.md §7.2）。
func parseIgnored(record []byte) (*FileStatus, error) {
	s := string(record)
	path := strings.TrimPrefix(s, "! ")
	if path == s {
		// 无前缀空格的异常，直接使用整条。
		path = s
	}
	return &FileStatus{
		Kind:    "!",
		Path:    path,
		Ignored: true,
	}, nil
}

// splitPrefixFields 在 record 中按空格切分前 n 个字段，返回 n+1 个字段
// （第 n+1 个为剩余字节，可能含空格，如文件路径）。
func splitPrefixFields(record []byte, n int) ([][]byte, bool) {
	fields := make([][]byte, 0, n+1)
	idx := 0
	for i := 0; i < n; i++ {
		// 跳过前导空格（理论上不应有）。
		for idx < len(record) && record[idx] == ' ' {
			idx++
		}
		sp := bytes.IndexByte(record[idx:], ' ')
		if sp == -1 {
			return nil, false
		}
		fields = append(fields, record[idx:idx+sp])
		idx += sp + 1
	}
	// 剩余作为最后字段（可能含空格）。
	// 跳过末尾的路径分隔空格已由上面 idx 指向第一个路径字节。
	fields = append(fields, record[idx:])
	return fields, true
}

// normalizeXY 将 porcelain v2 的 XY 两字符状态码中的 '.' 归一化为 ' '。
// 返回 byte 形式便于调用方比较。
func normalizeXY(xy string) (byte, byte) {
	if len(xy) < 2 {
		return ' ', ' '
	}
	x := byte(' ')
	if xy[0] != '.' {
		x = xy[0]
	}
	y := byte(' ')
	if xy[1] != '.' {
		y = xy[1]
	}
	return x, y
}
