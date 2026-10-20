// sse.go 实现 v2 SSE 帧解析（GET /api/event 的 text/event-stream；帧解析器
// 与 v1 同构——真实 opencode 无 event: 行，type 在 JSON payload 内。D1 允许
// transport 小件包级复用，v1 包冻结不可 import，此处按 v1 逐字镜像）。
package opencode2

import (
	"bufio"
	"context"
	"errors"
	"io"
)

// sseRawEvent 是从 SSE 流中解析出的单个事件原始数据。Type 来自 `event:` 行；
// data 为所有 `data:` 行以 \n 连接后的 bytes。
type sseRawEvent struct {
	Type string
	data []byte
}

// sseParser 按 SSE 协议逐事件解析：事件以空行分隔；多 `data:` 行以 \n 连接；
// `:` 注释行（heartbeat）忽略；id:/retry: 不使用；Next 阻塞读取，ctx 取消
// 返回 ctx.Err()（底层 Body 随 connCtx 取消返回错误，解除阻塞）。
type sseParser struct {
	r *bufio.Reader
}

func newSSEParser(r io.Reader) *sseParser {
	return &sseParser{r: bufio.NewReader(r)}
}

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
					// EOF 且无残余行：若已累积事件内容则派发（容忍末尾缺空行）。
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

		line = trimLineEnd(line)

		if line == "" {
			if ev.Type == "" && len(dataLines) == 0 {
				continue
			}
			if len(dataLines) > 0 {
				ev.data = joinDataLines(dataLines)
			}
			return ev, nil
		}

		if line[0] == ':' {
			continue
		}

		field, value, ok := splitSSEField(line)
		if !ok {
			continue
		}
		switch field {
		case "event":
			ev.Type = value
		case "data":
			dataLines = append(dataLines, value)
		default:
		}
	}
}

// splitSSEField 按 SSE 规范拆分 field:value：冒号后紧跟一个空格则跳过。
func splitSSEField(line string) (field, value string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] == ':' {
			field = line[:i]
			value = line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			return field, value, true
		}
	}
	return "", "", false
}

func trimLineEnd(line string) string {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}

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
