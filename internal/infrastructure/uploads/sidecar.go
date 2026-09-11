package uploads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appuploads "ocdeck/internal/application/uploads"
)

// sidecar 六键 schema（design D4 唯一持久真值；序列化始终输出全部键，字段顺序即
// JSON 键序）：
//
//	{"uploadId":"<32hex>","taskId":"<id>","connId":"<uuid>","filename":"<最终文件名>",
//	 "uploadedAt":"<RFC3339Nano UTC>","lastDeliveredAt":"<RFC3339Nano UTC>|null"}
//
// 解码须区分「键缺失」与「键存在且为 null」：lastDeliveredAt 为 JSON null 合法
//（从未投递的常态）；其余五键必须有非空字符串值，缺失/null/类型非法均损坏。
type sidecarJSON struct {
	UploadID        string  `json:"uploadId"`
	TaskID          string  `json:"taskId"`
	ConnID          string  `json:"connId"`
	Filename        string  `json:"filename"`
	UploadedAt      string  `json:"uploadedAt"`
	LastDeliveredAt *string `json:"lastDeliveredAt"` // nil → JSON null
}

// errSidecarReadFailed sidecar 读取失败（权限/I/O）：不归孤儿、不删、下轮重试
//（与内容损坏区分，spec「保留生命周期与回收」读取失败语义）。
var errSidecarReadFailed = errors.New("uploads: sidecar read failed")

// errSidecarCorrupt sidecar 内容损坏（键缺失/类型非法/时间格式非法/配对不变量破坏）。
var errSidecarCorrupt = errors.New("uploads: sidecar corrupt")

// writeSidecarTmp 将 meta 序列化为六键 JSON 写入 path（0600；调用方负责原子 rename）。
func writeSidecarTmp(path string, meta appuploads.Meta) error {
	raw := sidecarJSON{
		UploadID:   meta.UploadID,
		TaskID:     meta.TaskID,
		ConnID:     meta.ConnID,
		Filename:   meta.Filename,
		UploadedAt: meta.UploadedAt.UTC().Format(time.RFC3339Nano),
	}
	if meta.LastDeliveredAt != nil {
		last := meta.LastDeliveredAt.UTC().Format(time.RFC3339Nano)
		raw.LastDeliveredAt = &last // nil → null（schema 唯一合法未投递形态）
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return nil
}

// readSidecar 读取并解码 sidecar。
//   - 读取失败（打开/读 I/O）→ errSidecarReadFailed（下轮重试，不删）；
//   - JSON 解析失败/键缺失/格式非法 → errSidecarCorrupt；
//   - 成功返回规范化 Meta（时间一律 UTC）。
func (s *Store) readSidecar(path string) (appuploads.Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return appuploads.Meta{}, fmt.Errorf("%w: %v", errSidecarReadFailed, err)
	}
	return decodeSidecar(data, filepath.Base(path))
}

// decodeSidecar 按 schema 解码六键。用 map[string]json.RawMessage 区分「键缺失」
// 与「键存在且为 null」：六键任一缺失均损坏；lastDeliveredAt 为 JSON null →
// Meta.LastDeliveredAt 保持 nil（从未投递合法态），为字符串 → RFC3339Nano，
// 其余类型 → 损坏。五个必填键 null、类型非法、空值也损坏。未知键不拒绝（契约未规定）。
func decodeSidecar(data []byte, name string) (appuploads.Meta, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return appuploads.Meta{}, fmt.Errorf("%w: %v", errSidecarCorrupt, err)
	}
	str := func(key string) (string, bool) {
		v, ok := raw[key]
		if !ok || bytes.Equal(v, []byte("null")) {
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return "", false // 类型非法（数字/布尔/数组/对象）
		}
		return s, true
	}

	uploadID, ok := str("uploadId")
	if !ok || uploadID == "" {
		return appuploads.Meta{}, fmt.Errorf("%w: uploadId in %s", errSidecarCorrupt, name)
	}
	taskID, ok := str("taskId")
	if !ok || taskID == "" {
		return appuploads.Meta{}, fmt.Errorf("%w: taskId in %s", errSidecarCorrupt, name)
	}
	connID, ok := str("connId")
	if !ok || connID == "" {
		return appuploads.Meta{}, fmt.Errorf("%w: connId in %s", errSidecarCorrupt, name)
	}
	filename, ok := str("filename")
	if !ok || filename == "" {
		return appuploads.Meta{}, fmt.Errorf("%w: filename in %s", errSidecarCorrupt, name)
	}
	uploadedAtRaw, ok := str("uploadedAt")
	if !ok || uploadedAtRaw == "" {
		return appuploads.Meta{}, fmt.Errorf("%w: uploadedAt in %s", errSidecarCorrupt, name)
	}
	uploadedAt, err := parseRFC3339Nano(uploadedAtRaw)
	if err != nil {
		return appuploads.Meta{}, fmt.Errorf("%w: uploadedAt in %s: %v", errSidecarCorrupt, name, err)
	}
	meta := appuploads.Meta{
		UploadID:   uploadID,
		TaskID:     taskID,
		ConnID:     connID,
		Filename:   filename,
		UploadedAt: uploadedAt,
	}
	// lastDeliveredAt 六键之一：键缺失即损坏（delta spec:180）；null → 合法未投递态；
	// 字符串 → RFC3339Nano；其余类型/非法 → 损坏。
	v, ok := raw["lastDeliveredAt"]
	if !ok {
		return appuploads.Meta{}, fmt.Errorf("%w: lastDeliveredAt missing in %s", errSidecarCorrupt, name)
	}
	if !bytes.Equal(v, []byte("null")) {
		var lastRaw string
		if json.Unmarshal(v, &lastRaw) != nil {
			return appuploads.Meta{}, fmt.Errorf("%w: lastDeliveredAt type in %s", errSidecarCorrupt, name)
		}
		last, err := parseRFC3339Nano(lastRaw)
		if err != nil {
			return appuploads.Meta{}, fmt.Errorf("%w: lastDeliveredAt in %s: %v", errSidecarCorrupt, name, err)
		}
		meta.LastDeliveredAt = &last
	}
	return meta, nil
}

// parseRFC3339Nano 解析 RFC3339Nano 并规范化为 UTC。
func parseRFC3339Nano(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// sidecarValid 校验配对不变量（design D4，任一不符 = 损坏 sidecar）：
// taskId==目录名；filename==扫描位置同名配对文件 basename 且符合受管命名；
// uploadId==文件名 32hex 前缀。connId 非空。
func sidecarValid(meta appuploads.Meta, taskID, pairedName string) bool {
	if meta.TaskID != taskID {
		return false
	}
	if meta.Filename != pairedName {
		return false
	}
	if !managedNameRule.MatchString(pairedName) {
		return false
	}
	if !appuploads.IsUploadID(meta.UploadID) || !strings.HasPrefix(pairedName, meta.UploadID) {
		return false
	}
	if meta.ConnID == "" {
		return false
	}
	return true
}
