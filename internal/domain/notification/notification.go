// Package notification 定义通知能力的渠道无关值对象与契约（task-notifications
// design D1/D4）。本包只依赖 stdlib。
//
// 提供：
//   - Intent：渠道无关通知意图（类别详情由内容组装进入正文，意图不单独携带详情字段）
//   - Category/Level：通知类别与级别枚举（字面量为 SSE 契约取值）
//   - Capability：渠道能力位掩码（Group/Replace/Withdraw；Withdraw 本期无实现、位预留）
//   - Result/Channel/ChannelConfig/ResolvedChannel/DispatchPlan：渠道投递契约
//   - Config：通知配置模型与磁盘 schema（spec「通知配置存储」）
package notification

import "context"

// Category 通知类别（枚举：question|permission|idle|retry|error|test）。
type Category string

const (
	CategoryQuestion   Category = "question"
	CategoryPermission Category = "permission"
	CategoryIdle       Category = "idle"
	CategoryRetry      Category = "retry"
	CategoryError      Category = "error"
	CategoryTest       Category = "test"
)

// Level 通知级别（枚举：passive|active|timeSensitive|critical）。类别→级别映射
// 归 application/notification 的内容组装（design D4），本包不实现。
type Level string

const (
	LevelPassive       Level = "passive"
	LevelActive        Level = "active"
	LevelTimeSensitive Level = "timeSensitive"
	LevelCritical      Level = "critical"
)

// Intent 渠道无关通知意图（design D4）。类别详情由内容组装进入 Body，意图不单独
// 携带详情字段；URL 为目标页深链（推导规则 design D8，行为唯一表述 spec
//「通知内容与跳转链接」）。
type Intent struct {
	TaskID   string
	TaskName string
	Category Category
	Level    Level
	Title    string
	Body     string
	URL      string
}

// Result 单渠道投递结果（design D4）。Err 为失败原因摘要（渠道实现自行裁剪，
// MUST NOT 携带 token 等敏感明文）。
type Result struct {
	OK  bool
	Err string
}

// Capability 渠道能力位掩码（design D4）：CapGroup（分组）/ CapReplace（同键替换）/
// CapWithdraw（撤回，本期无渠道实现）。无能力渠道取 0（如 macos 的 osascript 实现，
// 由 dispatch 层给标题加任务名前缀降级）。能力位矩阵的唯一表述在 spec
//「通知渠道投递与降级」。
type Capability int

const (
	CapGroup Capability = 1 << iota
	CapReplace
	CapWithdraw
)

// ChannelConfig 渠道投递配置（design D4：候选判定时从配置快照固化，经 DispatchPlan
// 下发；渠道实现 MUST NOT 自行读取配置存储）。当前仅 bark 使用（endpoint 为剔除
// 尾部 '/' 后的推送端点、token 为 device key）；web/macos 无配置字段。
type ChannelConfig struct {
	Endpoint string
	Token    string
}

// Channel 通知渠道抽象（design D4）。渠道「已配置」判定与各实现能力位矩阵的唯一
// 表述在 spec「通知渠道投递与降级」；实现 MUST 只持静态依赖（http.Client、exec
// runner、WebPublisher），MUST NOT 持有或读取配置 Store。
type Channel interface {
	Name() string
	Caps() Capability
	Send(ctx context.Context, in Intent, cfg ChannelConfig) Result
}

// ResolvedChannel 已启用且已配置渠道的投递固化（渠道 + 该次投递使用的配置）。
type ResolvedChannel struct {
	Channel Channel
	Config  ChannelConfig
}

// DispatchPlan 投递配置固化（design D4）：门禁通过后生成，单次投递全过程 MUST 只
// 使用本计划内的固化值（URL/LLM 开关/渠道配置），投递期间的配置变更只影响下一次。
type DispatchPlan struct {
	URL        string
	LLMSummary bool
	Channels   []ResolvedChannel
}
