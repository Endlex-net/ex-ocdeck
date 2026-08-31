// Bark 推送渠道适配器（spec「Bark 渠道」/design D4）。
//
// 渠道只持静态 http.Client（超时/禁重定向在构造时固定），endpoint/token 经
// ChannelConfig 由 DispatchPlan 固化下发，MUST NOT 读取配置 Store。token 只存在
// 于请求体 device_key；请求体与 Bark 响应原文 MUST NOT 写入日志（失败 Err 仅含
// status/code 等判定要素，见各错误分支）。
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

	"ocdeck/internal/domain/notification"
)

// Bark wire 契约常量（spec「Bark 渠道」）。
const (
	barkTimeout        = 10 * time.Second
	barkMaxResponseLen = 64 * 1024 // 响应体读取上界，超限判定失败
)

// BarkChannel Bark 推送渠道。Caps=Group（spec「通知渠道投递与降级」矩阵）。
type BarkChannel struct {
	client *http.Client
}

// NewBarkChannel 生产构造：10s 超时、禁跟随重定向（3xx 走非 2xx 失败路径，
// 不自动重试——单次 Send 仅一次 Do）。
func NewBarkChannel() *BarkChannel {
	return newBarkChannel(barkTimeout)
}

func newBarkChannel(timeout time.Duration) *BarkChannel {
	return &BarkChannel{client: &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *BarkChannel) Name() string { return "bark" }

func (c *BarkChannel) Caps() notification.Capability { return notification.CapGroup }

// barkRequest wire 请求体：MUST 包含 device_key/title/body/level/group/url
// （device_key=token、group=ocdeck/<任务名>、url=Intent.URL，level 取 Intent
// 已映射级别）。
type barkRequest struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Level     string `json:"level"`
	Group     string `json:"group"`
	URL       string `json:"url"`
}

// groupKeyMaxNameLen 分组键中任务名截断上界（spec「Bark 渠道」：40 字符，rune）。
const groupKeyMaxNameLen = 40

// groupKey Bark group 与 terminal-notifier -group 共用的分组键（spec「Bark 渠道」
// 「macOS 本地通知渠道」）：`ocdeck/<任务名>`，任务名截 40 字符（rune）；任务名
// 为空回退任务 ID。分组名用户可见，MUST 可读且自带来源标识。web tag 不走本键
// （用户不可见，保持任务 ID 仅作替换去重）。
func groupKey(in notification.Intent) string {
	if in.TaskName == "" {
		return in.TaskID
	}
	r := []rune(in.TaskName)
	if len(r) > groupKeyMaxNameLen {
		r = r[:groupKeyMaxNameLen]
	}
	return "ocdeck/" + string(r)
}

// barkResponse wire 响应判定字段：指针区分「缺 code 键」与 code=0，两者均失败。
type barkResponse struct {
	Code *int64 `json:"code"`
}

// Send 向 <endpoint>/push POST JSON。成功判定：HTTP 2xx 且响应 JSON code==200；
// 非 2xx / 响应超 64KiB / 非法 JSON / 缺 code / code 非 200 均判定失败。
func (c *BarkChannel) Send(ctx context.Context, in notification.Intent, cfg notification.ChannelConfig) notification.Result {
	payload, err := json.Marshal(barkRequest{
		DeviceKey: cfg.Token,
		Title:     in.Title,
		Body:      in.Body,
		Level:     string(in.Level),
		Group:     groupKey(in),
		URL:       in.URL,
	})
	if err != nil {
		// 固定文案：错误信息不含请求体（token 在其中）。
		return notification.Result{OK: false, Err: "bark: marshal request failed"}
	}
	// endpoint 尾部 '/' 拼接前剔除（spec wire 契约）；URL 只含 endpoint，无 token。
	pushURL := strings.TrimRight(cfg.Endpoint, "/") + "/push"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pushURL, bytes.NewReader(payload))
	if err != nil {
		return notification.Result{OK: false, Err: fmt.Sprintf("bark: build request failed: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return notification.Result{OK: false, Err: fmt.Sprintf("bark: http request failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return notification.Result{OK: false, Err: fmt.Sprintf("bark: unexpected status %d", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, barkMaxResponseLen+1))
	if err != nil {
		return notification.Result{OK: false, Err: "bark: read response failed"}
	}
	if len(data) > barkMaxResponseLen {
		return notification.Result{OK: false, Err: "bark: response body exceeds 64KiB limit"}
	}
	var br barkResponse
	if err := json.Unmarshal(data, &br); err != nil || br.Code == nil {
		// 固定文案：json 错误信息可能携带响应字符片段，禁入日志（spec 响应原文禁日志）。
		return notification.Result{OK: false, Err: "bark: invalid response JSON or missing code"}
	}
	if *br.Code != 200 {
		return notification.Result{OK: false, Err: fmt.Sprintf("bark: response code %d", *br.Code)}
	}
	return notification.Result{OK: true}
}
