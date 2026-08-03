## MODIFIED Requirements

### Requirement: env 基线与宿主隔离
任务进程环境 MUST NOT 继承 ocdeck 服务端宿主 env。注入基线 SHALL 仅为最小基础集：`TERM/COLORTERM/HOME/USER/PATH/SHELL/LANG/LC_ALL/LC_CTYPE/TMPDIR/SSH_AUTH_SOCK` 与代理变量（`HTTP_PROXY/HTTPS_PROXY/NO_PROXY`，若宿主存在）。locale 变量（`LANG/LC_ALL/LC_CTYPE`）仅透传宿主非空值；三者均未设置或为空时系统 MUST 注入默认 `LANG=en_US.UTF-8`（保证 tmux attach 客户端 `client_utf8=1`，CJK 输出不被转写为 `_`）。执行 tmux 命令时 MUST 以清洗后的 env 调用，防止宿主 env 经 tmux server 全局环境隐式流入会话。provider 凭据主通道为 opencode 自身 auth store；需要 env 凭据的场景由用户经项目级/任务级 env 显式配置。

#### Scenario: 宿主变量不流入
- **WHEN** ocdeck 服务端 env 中存在 AWS_SECRET_ACCESS_KEY 等未配置变量，任务激活
- **THEN** 任务进程环境中不存在该变量（基础集除外）

#### Scenario: 高位 locale 变量原样透传
- **WHEN** 宿主设置了非空 LC_ALL 或 LC_CTYPE（LANG 未设置）
- **THEN** 该变量原样出现在任务进程与 tmux 命令环境中，系统不再注入默认 LANG

#### Scenario: 无有效 locale 时注入默认
- **WHEN** 宿主 LANG/LC_ALL/LC_CTYPE 均未设置或为空（如 launchd 启动）
- **THEN** 任务进程与 tmux 命令环境注入 `LANG=en_US.UTF-8`
