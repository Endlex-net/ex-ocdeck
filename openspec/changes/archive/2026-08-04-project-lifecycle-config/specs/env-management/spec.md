## MODIFIED Requirements

### Requirement: 明文存储与日志红线

环境变量在 SQLite 中明文存储（个人自用场景），DB 文件权限 MUST 为 0600。env 值 MUST NOT 出现在 **ocdeck 自身生成的日志与错误信息** 中（含服务端日志、API 错误响应、notice、last_error）。用户生命周期脚本（init / pre_delete）的 stdout/stderr 属于用户可控输出，ocdeck 按原样捕获落盘（见 project-lifecycle-config spec 的生命周期日志要求），不以 env 红线过滤，但系统 UI MUST 在脚本编辑器旁提示"脚本输出会落盘，勿打印敏感凭据"。系统 UI SHALL 提示用户勿存放高敏感凭据。

#### Scenario: 敏感值提示

- **WHEN** 用户保存环境变量
- **THEN** 界面提示明文存储风险

#### Scenario: ocdeck 自身日志不含 env 值

- **WHEN** env 相关的服务端日志、API 错误或 notice 被生成
- **THEN** 其中不出现任何 env 值（键名可出现）

#### Scenario: 用户脚本输出按原样捕获

- **WHEN** 用户 init script 执行 `echo $FOO`（FOO 为已配置 env）
- **THEN** init.log 含脚本输出的值；此为用户可控输出，不视为违反红线
