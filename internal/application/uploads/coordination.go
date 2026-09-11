package uploads

import (
	"context"
	"sync"
)

// Coordination 提供 per-task 协调锁（design D5）：串行化同一 task 目录上的上传提交、
// 投递、清理与生命周期提交。锁顺序固定为 Manager 任务锁 → 协调锁；持协调锁的一侧
// （投递/finalize/清理）MUST NOT 反向获取 Manager 任务锁。
//
// *Coordination 结构性满足 internal/task.UploadCoordination 窄端口（方法名 Acquire
// 对齐），组合根可直接注入，无需适配器。
type Coordination struct {
	locks sync.Map // taskID -> *coordLock
}

type coordLock struct {
	mu sync.Mutex
}

// NewCoordination 构造 per-task 协调锁。
func NewCoordination() *Coordination {
	return &Coordination{}
}

// Acquire 阻塞获取 taskID 的协调锁，返回 release（幂等）。阻塞感知 ctx 取消：
// ctx 结束时以 ctx.Err() 返回，MUST NOT 吞掉调用方的取消。
func (c *Coordination) Acquire(ctx context.Context, taskID string) (func(), error) {
	v, _ := c.locks.LoadOrStore(taskID, &coordLock{})
	l := v.(*coordLock)
	if l.mu.TryLock() {
		return onceUnlock(l), nil
	}
	// 慢路径：后台 goroutine 参与抢锁，所有权经 channel 移交（与 Manager lockTaskWait 同型）。
	acquired := make(chan struct{})
	go func() {
		l.mu.Lock()
		close(acquired)
	}()
	select {
	case <-ctx.Done():
		// 本调用放弃等待；goroutine 拿到锁后立即释放（锁从未移交给调用方）。
		go func() {
			<-acquired
			l.mu.Unlock()
		}()
		return nil, ctx.Err()
	case <-acquired:
		return onceUnlock(l), nil
	}
}

// onceUnlock 返回幂等解锁闭包（每次持锁一个 Once，释放恰好一次）。
func onceUnlock(l *coordLock) func() {
	var once sync.Once
	return func() { once.Do(l.mu.Unlock) }
}
