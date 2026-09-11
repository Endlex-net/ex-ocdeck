package uploads

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// 七步准入与业务码（design D5「投递七步准入」，首个失败生效；顺序钉死不得调整）。
type DeliverErrorCode string

const (
	CodeNotFound     DeliverErrorCode = "not_found"
	CodeExpired      DeliverErrorCode = "expired"
	CodeForbidden    DeliverErrorCode = "forbidden"
	CodeTaskInactive DeliverErrorCode = "task_inactive"
	CodeWriteFailed  DeliverErrorCode = "write_failed"
	CodeInvalidInput DeliverErrorCode = "invalid_input"
)

// DefaultStallTimeout / DefaultCleanupInterval 生产默认值（design D4/D5）。
const (
	DefaultStallTimeout   = time.Hour
	DefaultCleanupInterval = time.Hour
)

// orphanAge 孤儿（无 sidecar 文件/单边 sidecar/损坏 sidecar/遗留 .partial/.tmp）
// 的统一回收年龄：modtime 超过 1h 才删，独立于 TTL（design D4）。
const orphanAge = time.Hour

// uploadIDPattern 受管上传 ID：32 个小写 hex（design D4 受管命名；基础设施与
// 编排共用本判定，单一真相源）。
var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// IsUploadID 报告 s 是否为合法受管上传 ID（32hex）。
func IsUploadID(s string) bool {
	return uploadIDPattern.MatchString(s)
}

// Config 编排所需的配置子集（来自 config.Config，组合根裁剪注入）。
type Config struct {
	// MaxBytes 单文件上传上限（字节）。写满 M+1 字节即判定超限。
	MaxBytes int64
	// Retention 投递成功后的保留时长（TTL）。<=0 表示关闭。
	Retention time.Duration
	// UploadDir 受管上传存储根目录（config.UploadDir，加载期已绝对化）。
	// Deliver 步骤⑦以 <UploadDir>/<taskID>/<受管名> 计算注入的最终绝对路径，
	// 与 infrastructure store 的物理布局一致。
	UploadDir string
}

// Options 构造 Orchestrator。
type Options struct {
	Cfg   Config
	Store StorePort
	Tasks TaskPort
	// Conns 可选：当前 TUI 连接归属端口（组合根接线点）。nil 时投递①与上传
	// connId 准入跳过判定（Lane B 接入后生效）。
	Conns ConnPort
	// Events 可选：task 事件订阅端口。nil 时 task.deleted 回收不启用。
	Events EventSubscriber
	// Clock 可选：TTL/提交时刻时钟。nil 时 time.Now（测试注入以验证过期判定）。
	Clock func() time.Time
	// StallTimeout 活跃上传停滞超时。0 时回退 DefaultStallTimeout。
	StallTimeout time.Duration
	// CleanupInterval 周期清理间隔。0 时回退 DefaultCleanupInterval。
	CleanupInterval time.Duration
	// Logf 可选：nil 时 log.Printf。
	Logf func(format string, args ...any)
}

// Orchestrator 上传提交/投递/回收编排。决策唯一归属本类型；
// 文件系统操作全部经 StorePort。
type Orchestrator struct {
	cfg          Config
	store        StorePort
	tasks        TaskPort
	conns        ConnPort
	events       EventSubscriber
	clock        func() time.Time
	stallTimeout time.Duration
	cleanupIv    time.Duration
	logf         func(format string, args ...any)

	coord *Coordination

	mu     sync.Mutex
	index  map[indexKey]Meta        // taskID+uploadID -> 已提交元数据（可投递索引）
	active map[string]*ActiveUpload // uploadID -> 活跃上传登记
	sub    EventSubscription

	wg       sync.WaitGroup
	stopOnce sync.Once
	// stopCh 由 Stop 关闭：后台 goroutine 的第二退出条件（Stop 先于 ctx 取消时仍可收敛）。
	stopCh chan struct{}
}

type indexKey struct {
	taskID   string
	uploadID string
}

// New 构造编排器。
func New(opts Options) *Orchestrator {
	o := &Orchestrator{
		cfg:          opts.Cfg,
		store:        opts.Store,
		tasks:        opts.Tasks,
		conns:        opts.Conns,
		events:       opts.Events,
		stallTimeout: opts.StallTimeout,
		cleanupIv:    opts.CleanupInterval,
		coord:        NewCoordination(),
		index:        make(map[indexKey]Meta),
		active:       make(map[string]*ActiveUpload),
		stopCh:       make(chan struct{}),
	}
	if o.stallTimeout <= 0 {
		o.stallTimeout = DefaultStallTimeout
	}
	if o.cleanupIv <= 0 {
		o.cleanupIv = DefaultCleanupInterval
	}
	if opts.Clock != nil {
		o.clock = opts.Clock
	} else {
		o.clock = time.Now
	}
	if opts.Logf != nil {
		o.logf = opts.Logf
	} else {
		o.logf = log.Printf
	}
	return o
}

// Coordination 暴露 per-task 协调锁（组合根注入 task.Manager.UploadCoordination，
// 使任务离开 active 的提交与投递/finalize 临界区互斥）。
func (o *Orchestrator) Coordination() *Coordination { return o.coord }

// --- 投递（七步准入，design D5；per-task 协调锁临界区内完成）---

// injectPathKey inject ctx 携带的受管最终绝对路径 key（私有类型防跨包伪造）。
type injectPathKey struct{}

// ContextWithInjectPath 将受管上传的最终绝对路径包进 ctx（Deliver 步骤⑦调用
// inject 前写入；Lane B 注入函数经 InjectPathFrom 读取后写入 PTY）。
func ContextWithInjectPath(ctx context.Context, absPath string) context.Context {
	return context.WithValue(ctx, injectPathKey{}, absPath)
}

// InjectPathFrom 返回 ctx 中的受管上传最终绝对路径；未设置时 ok=false。
func InjectPathFrom(ctx context.Context) (string, bool) {
	p, ok := ctx.Value(injectPathKey{}).(string)
	return p, ok
}

// Deliver 向任务 taskID 的当前终端投递 uploadID：
// 在 per-task 协调锁临界区内完成七步准入并调用 inject（bracketed paste 单次完整写入，
// inject 完整写入才返回 nil，Lane B 注入边界；inject ctx 经 ContextWithInjectPath
// 携带受管最终绝对路径）。inject 成功且 TTL 启用时 best-effort
// 刷新 lastDeliveredAt（刷新失败不影响 ok）。
//
// 返回 (true, "") 表示投递成功。基础设施故障（任务查询失败/文件 stat I/O 失败）返回
// (false, "")——零值 code 非业务码，调用方按连接 1011 收尾；本路径零 PTY 写入。
func (o *Orchestrator) Deliver(ctx context.Context, taskID, connID, uploadID string, inject func(ctx context.Context) error) (bool, DeliverErrorCode) {
	release, err := o.coord.Acquire(ctx, taskID)
	if err != nil {
		o.logf("uploads: deliver acquire coordination lock (task %s): %v", taskID, err)
		return false, ""
	}
	defer release()

	// ① 终端类型/连接当前性（非 TUI 或 connId 非当前 → forbidden；shell 合法 deliver 帧在此拒绝）。
	if o.conns != nil {
		current, found, err := o.conns.CurrentTUIConn(ctx, taskID)
		if err != nil {
			o.logf("uploads: deliver step1 query current conn (task %s): %v", taskID, err)
			return false, ""
		}
		if !found || current != connID {
			return false, CodeForbidden
		}
	}
	// ② 任务活跃性（挂起/删除 → task_inactive）。
	if err := o.checkTaskActive(ctx, taskID); err != nil {
		if errors.Is(err, errTaskQuery) {
			return false, ""
		}
		return false, CodeTaskInactive
	}
	// ③ uploadId 查找（按 uploadId 全局查找：uploadID 为 crypto/rand 32hex 全局唯一，
	// 索引中每个 uploadID 至多一条；任务 B 用任务 A 的 uploadId 由此进入 ④ 归属判定
	// 而非直接 not_found，七步顺序冻结）。
	o.mu.Lock()
	var meta Meta
	found := false
	for k, m := range o.index {
		if k.uploadID == uploadID {
			meta, found = m, true
			break
		}
	}
	o.mu.Unlock()
	if !found {
		return false, CodeNotFound
	}
	// ④ task/connId 归属。
	if meta.TaskID != taskID || meta.ConnID != connID {
		return false, CodeForbidden
	}
	// ⑤ TTL。
	if o.expired(meta, o.clock()) {
		return false, CodeExpired
	}
	// ⑥ 文件存在性与路径校验（物理缺失 → not_found；路径含反斜杠/控制字符 → invalid_input）。
	if hasForbiddenPathRunes(meta.Filename) {
		return false, CodeInvalidInput
	}
	existsFile, err := o.store.StatUpload(taskID, meta.Filename)
	if err != nil {
		o.logf("uploads: deliver stat %s/%s: %v", taskID, meta.Filename, err)
		return false, ""
	}
	if !existsFile {
		return false, CodeNotFound
	}
	// ⑦ 写入（bracketed paste 单次完整写；失败 → write_failed，零 PTY 写入）。
	// 注入内容为受管最终绝对路径（<UploadDir>/<taskID>/<受管名>，与物理落盘一致；
	// 客户端原始名不进入路径）。配置加载时对 UploadDir 的校验不能替代投递时复查：
	// 组合结果（含配置根/taskID/文件名）MUST 在注入前整体复查（design D6 路径安全），
	// 不合法 → invalid_input、零 PTY 写入、零文件 I/O。
	injectPath := filepath.Join(o.cfg.UploadDir, taskID, meta.Filename)
	if hasForbiddenPathRunes(injectPath) {
		o.logf("uploads: deliver inject path rejected (task %s, upload %s)", taskID, uploadID)
		return false, CodeInvalidInput
	}
	injectCtx := ContextWithInjectPath(ctx, injectPath)
	if err := inject(injectCtx); err != nil {
		o.logf("uploads: deliver inject %s/%s: %v", taskID, meta.Filename, err)
		return false, CodeWriteFailed
	}
	// 成功：TTL 启用时刷新保留期基准。刷新成功在同一协调临界区同步索引的
	// LastDeliveredAt（否则清理/TTL 仍用旧基准误判过期）；失败仅日志、磁盘与索引
	// 都保留上次持久化成功的基准，不影响投递结果；uploadedAt 不回退。
	if o.cfg.Retention > 0 {
		at := o.clock().UTC()
		if err := o.store.RefreshLastDelivered(taskID, meta, at); err != nil {
			o.logf("uploads: refresh lastDeliveredAt %s/%s: %v (retention keeps persisted base)", taskID, meta.Filename, err)
		} else {
			meta.LastDeliveredAt = &at
			o.mu.Lock()
			o.index[indexKey{taskID: taskID, uploadID: uploadID}] = meta
			o.mu.Unlock()
		}
	}
	return true, ""
}

// checkTaskActive 任务活跃性检查（Deliver ② 与上传初始准入 AdmitUpload 共用）。
// 任务不存在返回 ErrUploadTaskNotFound，非 active 返回 ErrUploadTaskInactive；
// 基础设施故障返回 errTaskQuery（调用方按基础设施故障处理，不得映射为业务码）。
func (o *Orchestrator) checkTaskActive(ctx context.Context, taskID string) error {
	exists, active, err := o.tasks.TaskActive(ctx, taskID)
	if err != nil {
		return errTaskQuery
	}
	if !exists {
		return ErrUploadTaskNotFound
	}
	if !active {
		return ErrUploadTaskInactive
	}
	return nil
}

// errTaskQuery 内部标记：TaskPort 查询失败（基础设施故障）。
var errTaskQuery = errors.New("uploads: task query failed")

// expired 判断 TTL 是否已到（now >= (lastDeliveredAt ?? uploadedAt)+Retention；
// now==expiresAt 即过期）。Retention 关闭时永不过期。
func (o *Orchestrator) expired(meta Meta, now time.Time) bool {
	if o.cfg.Retention <= 0 {
		return false
	}
	base := meta.UploadedAt
	if meta.LastDeliveredAt != nil {
		base = *meta.LastDeliveredAt
	}
	return !now.Before(base.Add(o.cfg.Retention))
}

// hasForbiddenPathRunes 路径安全判定（design D5 ⑥/D6：反斜杠或控制字符
//（rune<0x20 或 DEL）→ invalid_input）。步骤⑥校验索引中的文件名，步骤⑦校验
// filepath.Join 后的最终绝对路径（配置根/taskID 组合不得绕过）。
func hasForbiddenPathRunes(name string) bool {
	for _, r := range name {
		if r == '\\' || r < 0x20 || r == 0x7F {
			return true
		}
	}
	return false
}
