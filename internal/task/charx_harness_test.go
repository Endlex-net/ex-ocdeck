package task

// charx_harness_test.go 实现 opencode-v1-spi-provider 阶段 0 的 characterization
// harness（tasks.md 1.1）：以脚本化 fake opencode server 为固定上游，经真实
// *opencode.Client 驱动 Manager 公共入口（Activate/Suspend/Delete/Reconcile/
// UpdateTaskInfo/Attention/AgentStatusSnapshot），并把结果捕获为六观察面快照：
//
//  1. HTTP/Web DTO：attention 等 web DTO 的规范化 JSON（字段序无关值全等）
//  2. store 终态：tasks/task_sessions 行集排序字符串
//  3. process 调用序列：NewSession/KillSession 调用名与参数序列（mockProc 边界记录）
//  4. notice/last_error：精确文本与计数
//  5. session ownership：归属集合
//  6. SSE 应用后内存态：pending 集合、agentStatus 快照
//
// 快照中的动态值（fake server 端口、相邻死端口）经 norm 归一化为稳定 token，
// 使同一 fixture 的期望值可写成 golden 字面量；阶段 2/3 迁移后同一 fixture 重跑，
// 用 assertCharxMatches 对新旧路径输出做逐面比较（design「行为保持验证」节）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appagent "ocdeck/internal/application/agentprovider"
	apptask "ocdeck/internal/application/task"
	"ocdeck/internal/config"
	"ocdeck/internal/infrastructure/opencode"
	"ocdeck/internal/infrastructure/process"
)

// --- process 边界记录（观察面 3） ---

// charxProc 包装 mockProc，把 NewSession/KillSession 的操作尝试（含失败尝试）、
// 相关参数（cwd、凭据形状、argv——随机密码归一化为长度形状，端口经 norm 归一化）
// 与结果记入单一有序调用日志。
// killAfterNew > 0 时，前 N 次 NewSession 创建的会话立即移除（进程死亡脚本：
// waitServeReadyOrDead 判死 → 无 KillSession 直接换端口轮换）。
type charxProc struct {
	*mockProc
	mu           sync.Mutex
	log          []string
	newCount     int
	killAfterNew int
}

func newCharxProc() *charxProc {
	return &charxProc{mockProc: newMockProc()}
}

// charxAuthShape 把会话 env 中的凭据归一化为「来源标记/密码长度形状」
// （"none" / "derived/pw(<n>)" / "legacy/pw(<n>)"）。密码长度本身是行为相关量
// （v1 派生凭据 = 32 字符 base64url——形状漂移应被保护网捕获）；来源标记为
// D3 非秘密判定材料（C2c 裁决 1：派生形状为 golden 契约）。
func charxAuthShape(env map[string]string) string {
	pw, ok := env["OPENCODE_SERVER_PASSWORD"]
	if !ok || pw == "" {
		return "none"
	}
	kind := env[appagent.CredKindEnvKey]
	if kind == "" {
		return fmt.Sprintf("none-kind/pw(%d)", len(pw))
	}
	return fmt.Sprintf("%s/pw(%d)", kind, len(pw))
}

func (p *charxProc) NewSession(spec process.SessionSpec) error {
	err := p.mockProc.NewSession(spec)
	p.mu.Lock()
	if err != nil {
		p.log = append(p.log, fmt.Sprintf("new-err %s cwd=%s auth=%s argv=%s err=%v",
			spec.Name, spec.Dir, charxAuthShape(spec.Env), strings.Join(spec.CmdArgv, " "), err))
		p.mu.Unlock()
		return err
	}
	p.log = append(p.log, fmt.Sprintf("new %s cwd=%s auth=%s argv=%s",
		spec.Name, spec.Dir, charxAuthShape(spec.Env), strings.Join(spec.CmdArgv, " ")))
	p.newCount++
	n := p.newCount
	p.mu.Unlock()
	if p.killAfterNew > 0 && n <= p.killAfterNew {
		p.mockProc.mu.Lock()
		delete(p.mockProc.sessions, spec.Name)
		p.mockProc.mu.Unlock()
		p.mu.Lock()
		p.log = append(p.log, "died "+spec.Name)
		p.mu.Unlock()
	}
	return nil
}

func (p *charxProc) KillSession(name string) (process.KillResult, error) {
	res, err := p.mockProc.KillSession(name)
	p.mu.Lock()
	if err != nil {
		p.log = append(p.log, "kill-err "+name)
	} else {
		p.log = append(p.log, "kill "+name)
	}
	p.mu.Unlock()
	return res, err
}

func (p *charxProc) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.log...)
}

// --- harness 装配 ---

// charxEnvOpts 控制 harness 装配变体。
type charxEnvOpts struct {
	// retryRange=true 时端口区间改为 [M-2, M]（M 为「映射端口」，见 fakeMappedPort）：
	// M-2、M-1 为空闲死端口，M 空闲但经 factory 映射到 fake server——构成
	// servePortRetries=3 恰好覆盖「死端口×2 + fake 端口」的完整轮换序列。
	retryRange bool
	// deadPorts=true 时 OCFactory 仅把 M 映射到 fake server，其余端口按真实端口构造
	// client（无进程 → 连接拒绝）。默认（false）全部端口映射到 fake server。
	deadPorts bool
	// killAfterNew：前 N 次 NewSession 后会话立即死亡（进程死亡脚本）。
	killAfterNew int
}

type charxEnv struct {
	t          *testing.T
	srv        *fakeOCServer
	store      *mockStore
	proc       *charxProc
	wt         *mockWorktree
	m          *Manager
	pub        *p18Publisher
	fakeMapped int // retryRange 变体的映射端口（空闲、factory 指向 fake server）

	lifeCancel context.CancelFunc
	portRange  config.PortRange
}

// newCharxEnv 装配 characterization 环境：真实 OCClient → fake server。
func newCharxEnv(t *testing.T, opts charxEnvOpts) *charxEnv {
	t.Helper()
	// harness 固定 master secret（迁移导入防御比对/派生凭据 golden 依赖；测试
	// 内覆盖经 t.Setenv 仍可显式覆写——env 优先序）。
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	srv := newFakeOCServer(t)
	env := newCharxEnvOnServer(t, srv, newMockStore(), newCharxProc(), opts)
	return env
}

// newCharxEnvOnServer 在既有 server/store/proc 上装配 Manager（重启恢复 fixture
// 复用同一 store+proc 模拟 server 进程重启后状态延续）。
func newCharxEnvOnServer(t *testing.T, srv *fakeOCServer, store *mockStore, proc *charxProc, opts charxEnvOpts) *charxEnv {
	t.Helper()
	return newCharxEnvOnTaskStore(t, srv, store, store, proc, opts)
}

// newCharxEnvOnTaskStore 以 TaskStore 包装层装配（A5：notice CAS 冲突注入等
// failpoint fixture 用）：Manager.Store 与 lifecycle adapter 共用同一 TaskStore——
// 接口分派使包装层覆写的方法对两条写路径同时生效；ms 为底座 mockStore
// （harness 读取断言与 OwnerOf 归属反查用）。
func newCharxEnvOnTaskStore(t *testing.T, srv *fakeOCServer, ts TaskStore, ms *mockStore, proc *charxProc, opts charxEnvOpts) *charxEnv {
	t.Helper()
	// harness 固定 master secret（迁移导入防御比对/派生凭据 golden 依赖；测试
	// 内覆盖经 t.Setenv 仍可显式覆写——env 优先序）。
	t.Setenv(appagent.MasterSecretEnv, "charx-test-master-secret")
	proc.killAfterNew = opts.killAfterNew
	wt := newMockWorktree()
	// 缺省区间从 fake 端口 +1 起顺延，使 fixture 的端口分配对本机环境确定。
	pr := config.PortRange{Min: srv.port + 1, Max: srv.port + 3}
	// fakeMappedPort：retryRange 变体的「映射端口」——自身空闲（可被 allocatePort 分配），
	// 但 factory 把它映射到 fake server 的真实监听端口。fake server 实际监听端口被占用
	// 永远不会被分配，故用独立映射端口承载「轮换最终成功」的尝试；与 fake 端口间隔 4
	// 确保 retry 区间 [M-2, M] 不与 fake 端口相交（fake 端口会被 isPortFree 跳过）。
	fakeMapped := srv.port + 5
	if opts.retryRange {
		pr = config.PortRange{Min: fakeMapped - 2, Max: fakeMapped}
	}
	factory := func(_ string, port int, password string, o appagent.Options) OCClient {
		// 默认：全部端口映射到 fake server（fixture 上游唯一）。
		return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(o))
	}
	if opts.deadPorts {
		// 端口重试变体：仅映射端口指向 fake server，其余按真实端口构造——
		// 死端口上健康检查连接拒绝 → 端口轮换直至命中映射端口。
		factory = func(_ string, port int, password string, o appagent.Options) OCClient {
			if port == fakeMapped {
				return opencode.NewClient(srv.port, password, opencode.OptionsFromNeutral(o))
			}
			return opencode.NewClient(port, password, opencode.OptionsFromNeutral(o))
		}
	}
	cfg := &config.Config{DataDir: t.TempDir(), ServePortRange: pr, ShutdownPolicy: config.ShutdownPersist}
	// 与生产装配对齐注入 LifecycleService（cmd/ocdeck-server main.go P1.4.4/P1.4.5
	// wiring）：align/claim/notice-CAS 经 application 编排，OwnerOf 归属反查可用
	//（历史重复行 fail-closed fixture 依赖）。p18AppAdapter 在 store 上实现全部
	// application ports；p18Publisher 记录领域事件供断言。
	pub := &p18Publisher{}
	appAdapter := &p18AppAdapter{mockAppAdapter: &mockAppAdapter{s: ts}, store: ms}
	lifecycleSvc := apptask.New(apptask.Options{
		Tasks:    appAdapter,
		Read:     appAdapter,
		Sessions: appAdapter,
		Publish:  pub,
	})
	m := New(Options{Cfg: cfg, Store: ts, Proc: proc, Worktree: wt, OCFactory: factory, Lifecycle: lifecycleSvc,
		ProviderRegistry: newTestRegistry()})
	// 短健康轮询与零退避：fixture 在秒级收敛（生产默认值不变，仅测试注入）。
	m.recoveryBackoffFn = func(int) time.Duration { return 0 }
	m.serveReadyTimeout = 60 * time.Millisecond
	m.serveReadyPollInterval = 5 * time.Millisecond
	m.probeColdStartBackoffFn = func() []time.Duration { return []time.Duration{time.Millisecond} }
	// 删除清理临时绑定入口（与生产 registry.Supervisor() 装配同形）；runtime
	// supervisor 保持 nil——charx 行为保持矩阵按 ora-4 约定锁定内联 golden
	// 锚定路径（双路径等价验证经 charx_dualpath_equiv_test.go 单独承载）。
	injectEphemeralSupervisorForTestWithPort(t, m, ms, proc, srv.port)
	lifeCtx, cancel := context.WithCancel(context.Background())
	m.SetLifecycleCtx(lifeCtx)
	t.Cleanup(cancel)
	return &charxEnv{t: t, srv: srv, store: ms, proc: proc, wt: wt, m: m, pub: pub,
		fakeMapped: fakeMapped, lifeCancel: cancel, portRange: pr}
}

// portHealthy 返回 fake server 的真实监听端口（迁移导入等直接与 srv.port 对接的
// fixture 用：legacy env 端口、binding data.port 断言——与契约测试的「健康检查
// 可达端口」语义一致）。
func (e *charxEnv) portHealthy() int { return e.srv.port }

// norm 把运行期动态端口归一化为稳定 token（快照/期望值可写成字面量）：
// fake 端口 → FAKE；fake-1..-3 → DEAD1..3（端口重试死端口）；fake+1..+3 → PT1..3
// （区间内顺延分配端口）。覆盖 argv（空格定界）、错误文本（":port"）与
// env/kv 形态（"=port" 后跟空格/逗号/右花括号）。
func (e *charxEnv) norm(in string) string {
	out := strings.ReplaceAll(in, fmt.Sprintf("127.0.0.1:%d", e.srv.port), "127.0.0.1:FAKE")
	replace := func(port int, token string) {
		p := fmt.Sprintf("%d", port)
		out = strings.ReplaceAll(out, ":"+p, ":"+token)
		out = strings.ReplaceAll(out, " "+p+" ", " "+token+" ")
		out = strings.ReplaceAll(out, "="+p+" ", "="+token+" ")
		out = strings.ReplaceAll(out, "="+p+",", "="+token+",")
		out = strings.ReplaceAll(out, "="+p+"}", "="+token+"}")
		out = strings.ReplaceAll(out, "="+p+"|", "="+token+"|")
	}
	replace(e.srv.port, "FAKE")
	for off := 1; off <= 3; off++ {
		replace(e.srv.port-off, fmt.Sprintf("DEAD%d", off))
		replace(e.srv.port+off, fmt.Sprintf("PT%d", off))
	}
	return out
}

// --- fixture 种子 ---

// charxSeedRepoWorktreeTask 种子 repo+worktree 任务（全量对齐组）。
func charxSeedRepoWorktreeTask(s *mockStore, taskID, projID string) TaskRow {
	s.seedProject(ProjectRow{ID: projID, Name: "p-" + projID, Path: "/repo-" + projID, DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projID, Name: "task " + taskID, Branch: "ocdeck/" + taskID,
		BaseRef: "refs/heads/main", Status: StatusSuspended,
		WorktreePath: "/data/worktrees/" + projID + "/" + taskID, Mode: TaskModeWorktree}
	s.tasks[taskID] = t
	return t
}

// charxSeedDirLocalPathTask 种子 dir+local-path 任务（ownedOnly 组；目录 = 项目路径）。
func charxSeedDirLocalPathTask(s *mockStore, taskID, projID, dir string) TaskRow {
	s.seedProject(ProjectRow{ID: projID, Name: "p-" + projID, Path: dir, DefaultBranch: "main", Kind: ProjectKindDir})
	t := TaskRow{ID: taskID, ProjectID: projID, Name: "task " + taskID,
		Status: StatusSuspended, WorktreePath: dir, Mode: TaskModeLocalPath}
	s.tasks[taskID] = t
	return t
}

// charxSeedRepoLocalPathTask 种子 repo+local-path 任务（ownedOnly 组；目录 = 项目路径）。
func charxSeedRepoLocalPathTask(s *mockStore, taskID, projID, dir string) TaskRow {
	s.seedProject(ProjectRow{ID: projID, Name: "p-" + projID, Path: dir, DefaultBranch: "main"})
	t := TaskRow{ID: taskID, ProjectID: projID, Name: "task " + taskID, Branch: "ocdeck/" + taskID,
		BaseRef: "refs/heads/main", Status: StatusSuspended,
		WorktreePath: dir, Mode: TaskModeLocalPath}
	s.tasks[taskID] = t
	return t
}

// --- 六观察面快照 ---

// charxSnapshot 是一次 fixture 运行后的六观察面捕获。字符串行集全部排序
// （process 序列除外：保持调用时序），便于新旧路径逐面 diff。
//
// 行集字段比较清单（A2：行为相关字段显式列出，防止捕获面静默漏检）：
//   - tasks：name/status/mode/anchor/last_port/delete_mode/last_error/notices/env(OCDECK_*)
//   - sessions：created/first_seen(<TS> 归一化)/last_seen/parent
//   - notices：taskID/code/message 精确文本逐条展开
type charxSnapshot struct {
	name string
	// ignore 显式声明跳过的观察面（面名→原因，面名见 assertCharxMatches）。
	// 未声明的面默认参与比较——want 零值意味着「期望为空」，不是「跳过」。
	ignore   map[string]string
	tasks    []string // "taskID|name=..|status|mode|anchor=<id|->|last_port=<n|->|delete_mode=<v|->|last_error=<text>|notices=[code:msg,...]|env={OCDECK_*}"
	sessions []string // "taskID|sessionID|created=<n>|first_seen=<TS>|last_seen=<n>|parent=<id|->"
	proc     []string // process 调用序列（时序保持，不排序）
	notices  []string // "taskID|code|message"（逐条展开，含计数语义）
	owner    []string // "taskID -> [s1 s2 ...]"（归属集合）
	dto      []string // 观察面 1/6：per-task "taskID|attention=<json>|agentStatus=<v>"
}

// captureCharx 捕获指定任务集的六观察面快照。store 读取失败立即报错
// （A2：不得以 continue 跳过残留行——那会把异常终态静默合法化）。
func (e *charxEnv) captureCharx(name string, taskIDs ...string) charxSnapshot {
	t := e.t
	t.Helper()
	snap := charxSnapshot{name: name}
	ctx := context.Background()

	for _, id := range taskIDs {
		row, err := e.store.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("charx capture: GetTask %s: %v（捕获失败不得跳过）", id, err)
		}
		anchor := "-"
		if row.AnchorSessionID.Valid {
			anchor = row.AnchorSessionID.String
		}
		lastPort := "-"
		if row.LastPort.Valid {
			lastPort = fmt.Sprintf("%d", row.LastPort.Int64)
		}
		deleteMode := "-"
		if row.DeleteMode.Valid {
			deleteMode = row.DeleteMode.String
		}
		lastErr := ""
		if row.LastError.Valid {
			lastErr = row.LastError.String
		}
		var noticeParts []string
		if entries, perr := parseNotices(row.Notice); perr == nil {
			for _, ne := range entries {
				noticeParts = append(noticeParts, ne.Code+":"+ne.Message)
				snap.notices = append(snap.notices, id+"|"+ne.Code+"|"+e.norm(ne.Message))
			}
		} else if row.Notice.Valid {
			t.Fatalf("charx capture: task %s notice JSON 损坏: %v", id, perr)
		}
		snap.tasks = append(snap.tasks, e.norm(strings.Join([]string{
			row.ID, "name=" + row.Name, row.Status, row.Mode, "anchor=" + anchor,
			"last_port=" + lastPort, "delete_mode=" + deleteMode,
			"last_error=" + lastErr, "notices=[" + strings.Join(sortedStrings(noticeParts), ",") + "]",
			"env=" + charxEnvProjection(row.EnvSnapshot),
		}, "|")))

		sessRows, err := e.store.ListTaskSessions(ctx, id)
		if err != nil {
			t.Fatalf("charx capture: ListTaskSessions %s: %v（捕获失败不得跳过）", id, err)
		}
		var owned []string
		for _, sr := range sessRows {
			parent := "-"
			if sr.ParentID != "" {
				parent = sr.ParentID
			}
			// FirstSeenAt 为本地观察墙钟（nowUnixI），归一化为 <TS> 常量标记。
			snap.sessions = append(snap.sessions, fmt.Sprintf("%s|%s|created=%d|first_seen=<TS>|last_seen=%d|parent=%s",
				id, sr.SessionID, sr.SessionCreatedAt, sr.LastSeenAt, parent))
			owned = append(owned, sr.SessionID)
		}
		snap.owner = append(snap.owner, id+" -> ["+strings.Join(sortedStrings(owned), " ")+"]")

		att, _ := e.m.Attention(id)
		agent := e.m.AgentStatusSnapshot(id)
		snap.dto = append(snap.dto, id+"|attention="+charxAttentionJSON(t, att)+"|agentStatus="+agent)
	}
	sort.Strings(snap.tasks)
	sort.Strings(snap.sessions)
	sort.Strings(snap.owner)
	sort.Strings(snap.dto)
	sort.Strings(snap.notices)
	snap.proc = e.normAll(e.proc.calls())
	return snap
}

// normAll 对调用序列逐行归一化。
func (e *charxEnv) normAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = e.norm(s)
	}
	return out
}

// charxEnvProjection 从 env_snapshot JSON 提取 OCDECK_* 键值（排序、确定性）。
// OCDECK_SERVE_PORT 的值统一遮蔽为 <PORT>：端口序列已由 process 调用序列（观察面 3）
// 与端口重试 fixture 覆盖，此处避免端口数字进入行集快照（端口值对环境敏感）。
// 快照不含宿主环境与随机密码——p1_gate 锁定 OPENCODE_SERVER_PASSWORD 不入快照。
func charxEnvProjection(snapshot sql.NullString) string {
	if !snapshot.Valid || snapshot.String == "" {
		return "{}"
	}
	var parsed struct {
		Vars map[string]string `json:"vars"`
	}
	if err := json.Unmarshal([]byte(snapshot.String), &parsed); err != nil {
		return "{corrupt}"
	}
	var keys []string
	for k := range parsed.Vars {
		if strings.HasPrefix(k, "OCDECK_") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := parsed.Vars[k]
		if k == "OCDECK_SERVE_PORT" {
			v = "<PORT>"
		}
		parts = append(parts, k+"="+v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// charxAttentionJSON 把实际 DTO（application.Attention，含 PendingPermission/
// PendingQuestion 全字段）序列化为字段序无关的规范 JSON（A1：不经自定义投影——
// 字段名、metadata 完整对象、question 独立字段均保留实际 DTO 形状）。
// 动态路径归一化（A7：路径精确）：仅 Permissions[*].Since 与 Questions[*].Since
// 两条路径替换为 <TS> 常量标记；metadata 内同名 Since 键属业务值，不遍历、
// 不归一化——其漂移必须被比较检出（见 TestCharx_AttentionJSON_SincePathOnly）。
// 实现路径：json.Marshal 实际 DTO → 解析为通用树（UseNumber 保留数字词法）→
// 按精确路径归一化 → 重排序 marshal（encoding/json 对 map 键按字典序输出，
// SetEscapeHTML(false) 保持 <TS> 可读字面量）。
func charxAttentionJSON(t *testing.T, att Attention) string {
	t.Helper()
	data, err := json.Marshal(att)
	if err != nil {
		t.Fatalf("charxAttentionJSON marshal: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree map[string]interface{}
	if err := dec.Decode(&tree); err != nil {
		t.Fatalf("charxAttentionJSON decode: %v", err)
	}
	charxNormalizeAttentionSince(tree)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(tree); err != nil {
		t.Fatalf("charxAttentionJSON remarshal: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// charxNormalizeAttentionSince 仅归一化 Permissions[*].Since / Questions[*].Since
// （A7：不递归进入其余键——尤其 metadata；调用方持无锁树）。
func charxNormalizeAttentionSince(tree map[string]interface{}) {
	for _, key := range []string{"Permissions", "Questions"} {
		items, ok := tree[key].([]interface{})
		if !ok {
			continue
		}
		for _, item := range items {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := m["Since"]; has {
				m["Since"] = "<TS>"
			}
		}
	}
}

// charxSurfaceNames 是 assertCharxMatches 认可的观察面名（ignore 声明用）。
var charxSurfaceNames = []string{
	"store/tasks", "store/sessions", "ownership", "notice", "dto/attention+agentStatus", "process",
}

// assertCharxMatches 六观察面逐面比较（golden 或阶段 2/3 新旧路径输出对比）。
// A2：默认要求完整快照比较——未声明 ignore 的面一律参与断言，want 零值面表示
// 「期望为空集」而非「跳过」；跳过某面必须在 want.ignore 中登记面名与原因。
func assertCharxMatches(t *testing.T, label string, want, got charxSnapshot) {
	t.Helper()
	if len(want.ignore) > 0 {
		for surface := range want.ignore {
			known := false
			for _, name := range charxSurfaceNames {
				if surface == name {
					known = true
					break
				}
			}
			if !known {
				t.Errorf("charx[%s] ignore 声明了未知面名 %q（合法面名：%v）", label, surface, charxSurfaceNames)
			}
		}
	}
	skipped := func(surface string) bool {
		_, ok := want.ignore[surface]
		return ok
	}
	// 行集比较（排序后逐行全等；nil 与空切片等价——期望为空集）。
	cmp := func(surface string, wantVals, gotVals []string) {
		if skipped(surface) {
			return
		}
		if !reflect.DeepEqual(sortedStrings(wantVals), sortedStrings(gotVals)) {
			t.Errorf("charx[%s] %s mismatch:\n want: %q\n got:  %q", label, surface, sortedStrings(wantVals), sortedStrings(gotVals))
		}
	}
	cmp("store/tasks", want.tasks, got.tasks)
	cmp("store/sessions", want.sessions, got.sessions)
	cmp("ownership", want.owner, got.owner)
	cmp("notice", want.notices, got.notices)
	cmp("dto/attention+agentStatus", want.dto, got.dto)
	// process 序列按时序比较（不排序）。
	if !skipped("process") && !reflect.DeepEqual(want.proc, got.proc) {
		t.Errorf("charx[%s] process-call-sequence mismatch:\n want: %q\n got:  %q", label, want.proc, got.proc)
	}
}

// charxWaitUntil 轮询直至 cond 为真或超时（fixture 同步屏障，替代 sleep 碰运气）。
func charxWaitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("charx: timeout waiting for %s", what)
}

// charxOwnedSessions 返回任务当前归属 sessionID 集合（断言 helper）。
func (e *charxEnv) charxOwnedSessions(taskID string) map[string]bool {
	rows, _ := e.store.ListTaskSessions(context.Background(), taskID)
	out := map[string]bool{}
	for _, r := range rows {
		out[r.SessionID] = true
	}
	return out
}

// charxTaskRow 读取任务行（fixture 断言 helper）。
func (e *charxEnv) charxTaskRow(taskID string) TaskRow {
	row, err := e.store.GetTask(context.Background(), taskID)
	if err != nil {
		e.t.Fatalf("GetTask %s: %v", taskID, err)
	}
	return row
}

// charxActivate 驱动 Activate，并把任务运行目录登记为 fake server 预期目录
// （A6 残留：/event 与 /session/status 的目录校验——登记先于 Activate，激活期
// 建立的 SSE 订阅与状态探测目录随之被校验）。
func (e *charxEnv) charxActivate(taskID string) error {
	if row, err := e.store.GetTask(context.Background(), taskID); err == nil {
		e.srv.expectDirectory(row.WorktreePath)
	}
	return e.m.Activate(context.Background(), taskID)
}

// charxNoticesOf 解析任务 notice 条目。
func (e *charxEnv) charxNoticesOf(taskID string) []noticeEntry {
	entries, err := parseNotices(e.charxTaskRow(taskID).Notice)
	if err != nil {
		return nil
	}
	return entries
}

// TestCharx_AttentionJSON_SincePathOnly（A7 有效性用例）：
//   - 仅 Permissions[*].Since / Questions[*].Since 被归一化——两 DTO 仅顶层 Since
//     不同时输出必须相等；
//   - metadata 内顶层与嵌套的 Since 键是业务值——不同值必须被比较检出（输出不等）。
func TestCharx_AttentionJSON_SincePathOnly(t *testing.T) {
	mk := func(metadata string, since int64) Attention {
		return Attention{Permissions: []PendingPermission{{
			PermissionRequest: opencode.PermissionRequest{
				ID: "p1", SessionID: "s1", Permission: "bash", Patterns: []string{"x"},
				Metadata: json.RawMessage(metadata),
			},
			Since: since,
		}}}
	}
	metaA := `{"Since":1,"nested":{"Since":2}}`
	outA := charxAttentionJSON(t, mk(metaA, 111))

	// 顶层 Since 不同 → 归一化后相等。
	if outB := charxAttentionJSON(t, mk(metaA, 999)); outB != outA {
		t.Errorf("顶层 Since 应被归一化：\n %s\n vs\n %s", outA, outB)
	}
	// metadata 顶层 Since 漂移 → 必须检出。
	if outC := charxAttentionJSON(t, mk(`{"Since":9,"nested":{"Since":2}}`, 111)); outC == outA {
		t.Errorf("metadata 顶层 Since 漂移未被检出：\n %s", outA)
	}
	// metadata 嵌套 Since 漂移 → 必须检出。
	if outD := charxAttentionJSON(t, mk(`{"Since":1,"nested":{"Since":7}}`, 111)); outD == outA {
		t.Errorf("metadata 嵌套 Since 漂移未被检出：\n %s", outA)
	}
	// Questions[*].Since 同样按路径归一化。
	q := func(since int64) Attention {
		return Attention{Questions: []PendingQuestion{{
			QuestionRequest: opencode.QuestionRequest{ID: "q1", SessionID: "s1",
				Questions: []opencode.QuestionItem{{Header: "h", Question: "q"}}},
			Since: since,
		}}}
	}
	if q1, q2 := charxAttentionJSON(t, q(1)), charxAttentionJSON(t, q(99)); q1 != q2 {
		t.Errorf("Questions[*].Since 应被归一化：\n %s\n vs\n %s", q1, q2)
	}
}
