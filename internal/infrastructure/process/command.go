// Package process 命令构造：tmux new-session 命令为单个 shell 字符串，
// MUST 从白名单 argv 逐元素单引号转义构造（design.md §2 命令构造）。
package process

import "strings"

// buildShellCommand 将白名单 argv 逐元素单引号转义后用空格拼成单个 shell 字符串。
// 转义规则（design.md §2）：每个元素用单引号包裹，元素内的 ' 替换为 '\''。
// 这样 tmux new-session 收到的命令字符串经 /bin/sh 解析后还原为原始 argv，
// 杜绝命令注入与未转义用户输入拼接。
//
// 空元素转义为 ''（空字符串参数）；元素含空格/特殊字符被单引号原样保留。
func buildShellCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// shellQuote 对单个字符串做单引号转义：' → '\''，整体用单引号包裹。
// 例：abc → 'abc'；a'b → 'a'\''b'；空 → ''。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}