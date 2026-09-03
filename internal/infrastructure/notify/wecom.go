// 企业微信群机器人 webhook 渠道适配器（spec「企业微信渠道」/design D6）。
//
// 渠道只持静态 http.Client（超时/禁重定向在构造时固定），完整 webhook URL 经
// ChannelConfig.Endpoint 由 DispatchPlan 固化下发，MUST NOT 读取配置 Store。
// webhook URL、请求体与企微响应原文 MUST NOT 写入日志（失败 Err 仅含判定要素，
// 见各错误分支；http.Client.Do/NewRequest 错误常含 URL，MUST NOT 用 %v 包裹）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ocdeck/internal/domain/notification"
)

// wecom wire 契约常量（spec「企业微信渠道」）。
const (
	wecomTimeout        = 10 * time.Second
	wecomMaxResponseLen = 64 * 1024 // 响应体读取上界，超限判定失败
	wecomMaxContentLen  = 4096      // markdown.content 字节上界（UTF-8）
)

// WecomChannel 企业微信群机器人渠道。Caps=0（spec「通知渠道投递与降级」矩阵）。
type WecomChannel struct {
	client *http.Client
}

// NewWecomChannel 生产构造：10s 超时、禁跟随重定向（3xx 走非 2xx 失败路径，
// 不自动重试——单次 Send 仅一次 Do）。
func NewWecomChannel() *WecomChannel {
	return newWecomChannel(wecomTimeout)
}

func newWecomChannel(timeout time.Duration) *WecomChannel {
	return &WecomChannel{client: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *WecomChannel) Name() string { return "wecom" }

func (c *WecomChannel) Caps() notification.Capability { return 0 }

// wecomRequest wire 请求体（spec「企业微信渠道」）：msgtype 固定 markdown，
// markdown.content 由意图逐字模板渲染后截断至 ≤4096 字节 UTF-8。
type wecomRequest struct {
	MsgType  string `json:"msgtype"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
}

// wecomResponse wire 响应判定字段：指针区分「缺 errcode 键」与 errcode=0，
// 两者均按失败处理（以 errcode 判定，MUST NOT 匹配 errmsg）。
type wecomResponse struct {
	ErrCode *int64 `json:"errcode"`
}

// Send 向 cfg.Endpoint 原样 POST JSON。成功判定：HTTP 2xx 且响应 JSON
// errcode==0；非 2xx / 响应超 64KiB / 非法 JSON / 缺 errcode / errcode 非 0
// 均判定失败。webhook URL 原样作为 POST 目标，MUST NOT 拼接 path、MUST NOT
// 剥离 query。
func (c *WecomChannel) Send(ctx context.Context, in notification.Intent, cfg notification.ChannelConfig) notification.Result {
	content := "**" + in.Title + "**\n" + in.Body
	if in.URL != "" {
		content += "\n\n[打开任务](" + in.URL + ")"
	}
	content = truncateUTF8Bytes(content, wecomMaxContentLen)

	req := wecomRequest{MsgType: "markdown"}
	req.Markdown.Content = content
	payload, err := json.Marshal(req)
	if err != nil {
		return notification.Result{OK: false, Err: "wecom: marshal request failed"}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		// 固定文案：NewRequest 错误常含 URL，禁入日志。
		return notification.Result{OK: false, Err: "wecom: build request failed"}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		// 固定文案：Do 错误常含 URL，禁入日志。
		return notification.Result{OK: false, Err: "wecom: http request failed"}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return notification.Result{OK: false, Err: fmt.Sprintf("wecom: unexpected status %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, wecomMaxResponseLen+1))
	if err != nil {
		return notification.Result{OK: false, Err: "wecom: read response failed"}
	}
	if len(data) > wecomMaxResponseLen {
		return notification.Result{OK: false, Err: "wecom: response body exceeds 64KiB limit"}
	}
	var wr wecomResponse
	if err := json.Unmarshal(data, &wr); err != nil || wr.ErrCode == nil {
		// 固定文案：json 错误信息可能携带响应字符片段，禁入日志。
		return notification.Result{OK: false, Err: "wecom: invalid response JSON or missing errcode"}
	}
	if *wr.ErrCode != 0 {
		return notification.Result{OK: false, Err: fmt.Sprintf("wecom: response errcode %d", *wr.ErrCode)}
	}
	return notification.Result{OK: true}
}

// truncateUTF8Bytes 从左按 rune 截断 content 至不超过 maxBytes 字节的有效 UTF-8。
// 若追加下一个 rune 会超过 maxBytes 则停止；MUST NOT 截断到半个 UTF-8 序列。
// 仅当字节长度达标且已是有效 UTF-8 时才直接返回：非法字节经 json.Marshal 会被
// 替换为 3 字节 U+FFFD，可能使 ≤4096 的输入在 marshal 后膨胀超限（spec「企业微信
// 渠道」content MUST ≤4096 字节 UTF-8）。非法字符串走 rune 扫描路径——Go range
// 将每个非法字节替换为 U+FFFD（RuneLen=3），按字节预算截断后再交由 marshal。
func truncateUTF8Bytes(content string, maxBytes int) string {
	if len(content) <= maxBytes && utf8.ValidString(content) {
		return content
	}
	var (
		out  strings.Builder
		size int
	)
	for _, r := range content {
		n := utf8.RuneLen(r)
		if n < 0 || size+n > maxBytes {
			break
		}
		out.WriteRune(r)
		size += n
	}
	return out.String()
}
