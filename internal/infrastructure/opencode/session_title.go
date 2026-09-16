// session_title.go 实现会话标题更新端点（openspec change task-info-editable D3）。
// 端点契约为目标契约声明（CONTRACT.md 增补行标注），以 live probe 为准的既有约定不变。
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrSessionTitleUnsupported 表示 serve 不提供会话标题更新端点（PATCH 404）。
// 404 二分取消（D3）：路由级 404（端点缺失）与会话级 404（会话已删）无法可靠区分，
// 一律归一为「不支持/不可用」，由调用方按能力降级处置（记 notice、不阻断改名）。
var ErrSessionTitleUnsupported = errors.New("opencode: session title update unsupported")

// UpdateSessionTitle PATCH /session/:id?directory=<wt>（task-info-editable D3 协议表）。
// body `{"title": <title>}`；成功 200 + Session JSON，校验顶层 id 与请求一致。
// 404 → ErrSessionTitleUnsupported（能力降级判定）；401 → ErrUnauthorized；
// 其他非 2xx → httpStatusError；网络/超时/解码失败 → 普通 error（调用方 best-effort 处置）。
func (c *Client) UpdateSessionTitle(ctx context.Context, dir, id, title string) error {
	q := url.Values{}
	q.Set("directory", dir)
	reqURL := c.baseURL + "/session/" + url.PathEscape(id) + "?" + q.Encode()

	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return fmt.Errorf("opencode: update session title: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrSessionTitleUnsupported
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode/100 != 2 {
		return &httpStatusError{status: resp.StatusCode, body: readBodyForError(resp.Body)}
	}
	var raw jsonRawObject
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fmt.Errorf("opencode: update session title: decode body: %w", err)
	}
	s, err := parseSession(raw)
	if err != nil {
		return fmt.Errorf("opencode: update session title: %w", err)
	}
	if s.ID != id {
		return fmt.Errorf("opencode: update session title: response id %q does not match request %q", s.ID, id)
	}
	return nil
}
