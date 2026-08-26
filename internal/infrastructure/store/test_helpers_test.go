// test_helpers.go 提供 store 测试内部 sql.NullString ↔ *string 转换（task P1.2）。
package store

import "database/sql"

// nsToPtr 把 sql.NullString 映射为 *string：Invalid → nil。供测试调用新签名 store 方法。
func nsToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	s := ns.String
	return &s
}