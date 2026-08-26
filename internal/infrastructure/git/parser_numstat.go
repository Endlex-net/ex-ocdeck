package git

import (
	"bytes"
	"strconv"
)

// numstatEntry 为单文件 numstat 的增删行数。二进制文件以 add/del = - 表示。
type numstatEntry struct {
	additions int
	deletions int
	isBinary  bool
}

// parseNumstatZ 解析 `git diff --numstat -z [--cached]` 的输出。
//
// -z 格式（条目以 NUL 分隔）：
//   - 普通文件：单个记录 "<add>\t<del>\t<path>"，路径为前两个 tab 之后到记录结尾的全部字节
//     （-z 关闭 core.quotePath 引号，故 tab/newline/非 ASCII/冒号均原样保留）。
//   - rename/copy：三个记录 "<add>\t<del>" + "<oldPath>" + "<newPath>"（stat 头无第三个字段）。
//   - 二进制："-\t-\t<path>"（普通）或 "-\t-" + old + new（rename）。
//
// 返回 byPath（按路径索引，rename 时同时登记 newPath）与 byRename（按 "old\x00new" 复合键索引）。
// 同路径多次出现时累加 add/del。
func parseNumstatZ(stdout []byte) (byPath map[string]*numstatEntry, byRename map[string]*numstatEntry) {
	byPath = make(map[string]*numstatEntry)
	byRename = make(map[string]*numstatEntry)

	records := bytes.Split(stdout, []byte{0})
	// -z 输出末尾有 trailing NUL，Split 产生空末尾记录，循环中跳过空记录。

	i := 0
	for i < len(records) {
		rec := records[i]
		if len(rec) == 0 {
			i++
			continue
		}
		// rec 至少含两个 tab 才可能是普通 stat 行；rename 头只有 "add\tdel"（一个 tab）。
		firstTab := bytes.IndexByte(rec, '\t')
		if firstTab == -1 {
			i++
			continue
		}
		secondTab := bytes.IndexByte(rec[firstTab+1:], '\t')
		if secondTab == -1 {
			// 仅 "add\tdel"（一个 tab）→ rename 头：接下两条记录为 old/new。
			addStr := string(rec[:firstTab])
			delStr := string(rec[firstTab+1:])
			binary := addStr == "-" || delStr == "-"
			add := numOrZero(addStr)
			del := numOrZero(delStr)

			if i+2 >= len(records) {
				break
			}
			oldPath := string(records[i+1])
			newPath := string(records[i+2])
			if len(oldPath) == 0 || len(newPath) == 0 {
				// 不完整的 rename 条目，跳过。
				i += 3
				continue
			}
			i += 3

			key := oldPath + "\x00" + newPath
			mergeRename(byRename, key, add, del, binary)
			mergeRename(byPath, newPath, add, del, binary)
			continue
		}

		// 普通文件："add\tdel\tpath"。
		addStr := string(rec[:firstTab])
		delStr := string(rec[firstTab+1 : firstTab+1+secondTab])
		path := string(rec[firstTab+1+secondTab+1:])
		i++

		if len(path) == 0 {
			continue
		}
		binary := addStr == "-" || delStr == "-"
		add := numOrZero(addStr)
		del := numOrZero(delStr)
		mergeInto(byPath, path, add, del, binary)
	}
	return byPath, byRename
}

func numOrZero(s string) int {
	if s == "-" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func mergeInto(m map[string]*numstatEntry, key string, add, del int, binary bool) {
	e := m[key]
	if e == nil {
		e = &numstatEntry{}
		m[key] = e
	}
	if binary {
		e.isBinary = true
		return
	}
	e.additions += add
	e.deletions += del
}

func mergeRename(m map[string]*numstatEntry, key string, add, del int, binary bool) {
	mergeInto(m, key, add, del, binary)
}
