package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrPermissionRequestGone 表示权限请求已了结（404 且 _tag=PermissionNotFoundError，
// 人工已在终端抢先批准/拒绝的竞态，task-permission-mode D6 失败分类 ③）。
// 调用方视为正常忽略：不报错、不重试。
var ErrPermissionRequestGone = errors.New("opencode: permission request not found")

// maxReplyBodyBytes 单响应体读取上限（1MB，与 ai 包请求/响应上限同量级；
// 回复响应体极小，上限仅作防御）。
const maxReplyBodyBytes = 1 << 20

// readReplyBody 完整读取有大小上限的响应体（task-permission-mode F1）。
// 分类（404 双语义）与成功判定 MUST 基于完整 body——共享的 readBodyForError 只做
// 单次 Read 且丢弃读取错误，分块到达或截断时会读到不完整片段，不能用于分类。
// 读取失败（超时/连接中断）或超限返回 err：调用方按 D6 分类 ②「结果未知」处置，
// MUST NOT 据此标记 unsupported。
func readReplyBody(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxReplyBodyBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxReplyBodyBytes {
		return "", fmt.Errorf("opencode: reply permission: response body exceeds %d bytes", maxReplyBodyBytes)
	}
	return string(data), nil
}

// ReplyPermission POST /permission/{requestID}/reply?directory=<dir>（opencode 1.18.18 契约，
// task-permission-mode D6）。body `{"reply": reply}`；reply 由调用方限定 once/reject
//（本特性 MUST NOT 传 always，spec 契约）。
//
// 结果映射（client.go 错误分类先例：区分 401/404/其他非 2xx）：
//   - 200 true → 成功（nil）；200 false、空体、非法 JSON、尾随内容 → 普通错误（发送结果未知，
//     task-permission-mode D6 分类 ②）；
//   - 401 → ErrUnauthorized（普通错误：凭据异常，调用方 MUST NOT 据此标记能力不可用）；
//   - 404 双语义（基于完整读取的响应体）：`_tag=PermissionNotFoundError` →
//     ErrPermissionRequestGone（竞态忽略）；其他 404（路由不存在=老版本无此端点）→
//     ErrCapabilityUnsupported（唯一允许调用方标记 reply-unsupported 的形态）；
//     响应体读取失败/超限 → 普通错误（结果未知，MUST NOT 标记 unsupported）；
//   - 其他 4xx/5xx/超时/连接失败 → 普通错误（结果未知：不重试、不补偿、不本地断言状态）。
func (c *Client) ReplyPermission(ctx context.Context, dir, requestID, reply string) error {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/permission/" + url.PathEscape(requestID) + "/reply?" + q.Encode()

	body, err := json.Marshal(map[string]string{"reply": reply})
	if err != nil {
		return fmt.Errorf("opencode: reply permission: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err // 超时/连接失败：结果未知（D6 分类 ②）
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		body, rerr := readReplyBody(resp.Body)
		if rerr != nil {
			// 完整读取失败（超时/截断/超限）：无法区分 Gone 与路由 404 → 结果未知
			//（D6 分类 ②），MUST NOT 标记 unsupported。
			return fmt.Errorf("opencode: reply permission: read 404 body: %w", rerr)
		}
		if permissionRequestGoneBody(body) {
			return ErrPermissionRequestGone
		}
		return ErrCapabilityUnsupported
	}
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	// 成功响应是 JSON true（DeleteSession 同款回包形状）；false/空体/非法 JSON/尾随
	// 内容 = 结果未知（json.Unmarshal 拒绝尾随第二个值与垃圾，F2）。
	respBody, rerr := readReplyBody(resp.Body)
	if rerr != nil {
		return fmt.Errorf("opencode: reply permission: read body: %w", rerr)
	}
	var ok bool
	if err := json.Unmarshal([]byte(respBody), &ok); err != nil {
		return fmt.Errorf("opencode: reply permission: decode body: %w", err)
	}
	if !ok {
		return fmt.Errorf("opencode: reply permission: unexpected body (not true)")
	}
	return nil
}

// permissionRequestGoneBody 判定 404 响应体是否 _tag=PermissionNotFoundError（已了结竞态）。
// 空体/非法 JSON/其他 tag → false（路由 404，能力降级语义）。
func permissionRequestGoneBody(body string) bool {
	var payload struct {
		Tag string `json:"_tag"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return false
	}
	return payload.Tag == "PermissionNotFoundError"
}
