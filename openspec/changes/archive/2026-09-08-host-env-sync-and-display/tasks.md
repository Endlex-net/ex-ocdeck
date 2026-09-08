# Tasks: host-env-sync-and-display

## 1. hostenv 包与 follow_host 兜底（design D1/D2/D3/D6）

- [x] 1.1 `internal/infrastructure/hostenv` 包：`Lookup`（进程非空 → 捕获缓存非空 → miss）、`Refresh`、懒加载捕获（RWMutex + double-check）、`captureMu` 捕获+发布串行化
- [x] 1.2 捕获协议：`printf BEGIN && /usr/bin/env -0 && printf END\0`（&& 链、显式 env 路径）；帧语法（BEGIN 首行、NUL 记录、END 独立控制记录精确匹配、尾部忽略）；成功判据（未超时/退出码零/完整帧/条目可解析/零合法条目失败）；5s 超时进程组 kill；stderr 丢弃 stdin 不连接
- [x] 1.3 条目合法性：首个 `=` 切分、无 `=`/空键忽略、`KEY=` 空值保留进缓存、重复键取首条；shell 回退：$SHELL →（linux: /etc/passwd 当前用户登录 shell → /bin/bash；darwin: /bin/zsh）
- [x] 1.4 缓存三态状态机（未加载/成功/失败 × 普通读取/Refresh 成败），失败不发布部分或成功空结果，日志红线
- [x] 1.5 接线收窄：follow_host 分支（activate.go layerEnvSnapshot）与 api hostEnvLookup 接 hostenv.Lookup；基础集/locale/userShell/notice hostEnv/main.go 保持 os.LookupEnv
- [x] 1.6 activate.go 形态校验（kind/mode/base_ref/branch）前移到宿主解析之前；guard 测试断言非法路径 capture 计数为 0
- [x] 1.7 hostenv 测试：帧协议（值/键含标记文本、无效帧表驱动测试含截断/控制记录缺失/零合法条目、空值保留、重复键取首条）、状态机迁移、并发 -race、parsePasswdShell 表单测；H1 回归与校验前移用例均经旧实现变红验证

## 2. 列举与刷新 API（design D4）

- [x] 2.1 `registerEnvRoutes` 新增 `GET /api/v1/env/host`：合并视图 `{vars:[{key,value,source}]}`，source=process/shell/both 按键存在性，值取进程非空→捕获非空→空串；未加载态首次调用触发捕获；捕获失败降级为进程视图正常返回
- [x] 2.2 `POST /api/v1/env/host/refresh`：调 hostenv.Refresh()；成功返回最新合并视图；失败返回 HTTP 500 code=internal 固定文案（不含捕获内容）；缓存迁移遵循 D3
- [x] 2.3 handler 测试：合并视图与 source 标注（含 both/仅 shell 空值键/双空值）、降级视图、refresh 成功/失败（internal/api/hostenv_api_test.go；合并路径经 mutation 验证：旁路缓存合并时用例变红；快照不变性由 2.4 guard 测试覆盖）
- [x] 2.4 快照与环境不变性验证：refresh 成功/失败前后已激活任务 env 快照与服务端基础环境（process manager/watchdog）不变；refresh 成功后下一次激活的 follow_host 解析使用新捕获缓存。证据：internal/task/hostenv_snapshot_guard_test.go 自动化覆盖（快照 JSON 与 DefaultBaseEnv 深比较不变 + 新缓存生效断言），-count=2/-shuffle=1 通过

## 3. 系统环境变量展示区 UI（design D5）

- [x] 3.1 api client 与类型：`getHostEnv()`、`refreshHostEnv()`、`HostEnvVar{key,value,source}`
- [x] 3.2 新组件 `web/src/components/SystemEnvPanel.tsx`：首次加载调用 getHostEnv()，等待期间展示 loading（首次调用可能触发捕获，D4），完成后呈现合并视图；列表行（key、定长掩码 `••••••••`、来源 badge、点击切换明文行级状态）、搜索框按 key 子串过滤、固定说明「刷新更新宿主解析结果；任务需挂起后激活生效。」；复用既有 `env-list`/`env-row`/`badge` 样式族
- [x] 3.3 刷新按钮：调 refreshHostEnv，进行中禁用 loading；成功替换列表并触发全局列表 reload；失败保留原列表+错误提示、不联动
- [x] 3.4 每行添加按钮：putGlobalEnv(key,'follow_host','')；按钮状态优先级 已配置→系统保留(OCDECK_*/OPENCODE_SERVER_PASSWORD)→非法 key→可添加，前三态禁用标注原因；成功后刷新全局列表
- [x] 3.5 SettingsPage env tab：全局环境变量 od-card 下方新增 od-card 承载 SystemEnvPanel；共享 reload 信号（回调或 key 递增）同步 GlobalEnvEditor 与 SystemEnvPanel
- [x] 3.6 UI 行为验收（组件测试或逐场景记录的手工验证，交付时写明证据）：默认掩码/点击显示、搜索过滤、添加按钮四态优先级（已配置→系统保留→非法 key→可添加）、刷新成功替换列表并联动全局列表、刷新失败保留原列表+错误提示、首次加载 loading

## 4. 文档与验证

- [x] 4.1 README 配置章节更新：login shell 兜底 + 系统环境变量展示/刷新说明（修正 fix-1 初版"需重启 server 生效"表述——刷新 API 落地后不再需重启；.bashrc 表述与 D2"遵循所选 shell 启动文件规则"一致；linux 缺省 shell 的 /etc/passwd 回退已同步）
- [x] 4.2 全量验证：go build/vet/test ./...（含 -race hostenv）、web `pnpm build`（已含 tsc --noEmit，无独立 typecheck script）
