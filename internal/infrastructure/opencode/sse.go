package opencode

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// sseRawEvent 是从 SSE 流中解析出的单个事件原始数据（§20 envelope 前的形态）。
// Type 来自 `event:` 行；data 为所有 `data:` 行拼接后的 bytes（按 SSE 规范以 \n 连接多行）。
type sseRawEvent struct {
	Type string
	data []byte
}

// sseParser 按 SSE 协议（text/event-stream）逐事件解析。仅依赖标准库 bufio。
//
// 处理要点（§20 与 SSE 规范）：
//   - 事件以空行分隔；单事件内可有多个 `data:` 行，以 \n 连接成完整 data。
//   - `event:` 行指定类型；缺省 event 字段时 Type 为空（心跳/注释）。
//   - `:` 开头为注释行（含 heartbeat），忽略。
//   - 其他字段（id:/retry:）本场景不使用，忽略。
//   - Next 阻塞读取下一个完整事件；ctx 取消时返回 ctx.Err()。
type sseParser struct {
	r *bufio.Reader
}

func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{r: bufio.NewReader(r)}
}

// Next 读取并返回下一个完整事件。返回的事件 data 可能为空（仅有 event: 行）。
// 遇到 io.EOF 视为流结束；ctx 取消返回 ctx.Err()。
//
// 注：bufio.Reader 本身不感知 ctx；通过在循环中检查 ctx.Done() 实现 cancellable 读取。
// 由于底层 reader（http.Response.Body）在 ctx 取消时会返回错误，读取最终会解除阻塞。
func (p *sseParser) Next(ctx context.Context) (sseRawEvent, error) {
	var ev sseRawEvent
	var dataLines []string

	for {
		if err := ctx.Err(); err != nil {
			return sseRawEvent{}, err
		}
		line, err := p.r.ReadString('\n')
		if err != nil {
			if len(line) == 0 {
				if errors.Is(err, io.EOF) {
					// EOF 且无残余行：若已累积事件内容则派发（容忍末尾缺空行分隔符）。
					if ev.Type != "" || len(dataLines) > 0 {
						if len(dataLines) > 0 {
							ev.data = joinDataLines(dataLines)
						}
						return ev, nil
					}
					return sseRawEvent{}, io.EOF
				}
				return sseRawEvent{}, err
			}
			// 有残留内容但无换行：作为最后一行处理（err 在下轮触发 EOF 收尾）。
		}

		// 去掉行尾 \r\n / \n。
		line = trimLineEnd(line)

		// 空行 = 事件分隔符：若已累积内容则派发事件。
		if line == "" {
			if ev.Type == "" && len(dataLines) == 0 {
				continue // 无内容的空行（如首行前），忽略
			}
			if len(dataLines) > 0 {
				ev.data = joinDataLines(dataLines)
			}
			return ev, nil
		}

		// 注释行（含 heartbeat）忽略。
		if line[0] == ':' {
			continue
		}

		field, value, ok := splitSSEField(line)
		if !ok {
			continue // 无冒号的行忽略
		}
		switch field {
		case "event":
			ev.Type = value
		case "data":
			dataLines = append(dataLines, value)
		default:
			// id/retry 等本场景不使用，忽略。
		}
	}
}

// splitSSEField 按 SSE 规范拆分 field:value：冒号后若有一个空格则跳过。
func splitSSEField(line string) (field, value string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == ':' {
			field = line[:i]
			value = line[i+1:]
			// SSE 规范：冒号后紧跟一个空格则跳过该空格。
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			return field, value, true
		}
	}
	return "", "", false
}

// trimLineEnd 去掉行尾 \r\n / \n / \r。
func trimLineEnd(line string) string {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}

// joinDataLines 按 SSE 规范以 \n 连接多行 data。
func joinDataLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	out := make([]byte, 0, len(lines)*16)
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}