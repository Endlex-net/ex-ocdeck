# Access Control Specification

## Purpose
以单个访问 token 保护 ocdeck 全部管理 API 与终端连接，默认仅绑定 loopback，满足个人自用的最小认证边界。

## Requirements

### Requirement: 单 token 认证
系统 SHALL 以单个访问 token 保护全部管理 API 与 WebSocket 终端连接。token 由用户在服务端配置（环境变量 OCDECK_TOKEN 或配置文件——**v1 实现仅 env 来源**，配置文件来源为已备案后续项），未配置时服务端 MUST 拒绝启动并提示。REST 使用 `Authorization: Bearer`；WebSocket 使用升级后首条消息认证，token MUST NOT 出现在 URL query 中。

#### Scenario: 携带有效 token 访问
- **WHEN** 请求携带正确 token
- **THEN** 请求被正常处理

#### Scenario: 未认证访问被拒
- **WHEN** 请求缺失或携带错误 token
- **THEN** 系统返回 401（WS 则关闭连接），不泄露任何资源信息

#### Scenario: 未配置 token 拒绝启动
- **WHEN** 服务端启动时未配置 token
- **THEN** 进程退出并提示必须配置访问 token

### Requirement: 网络绑定边界
服务端 SHALL 默认仅绑定 `127.0.0.1`；远程访问 MUST 依赖用户自管的 HTTPS 反代。token MUST NOT 写入日志。

#### Scenario: 默认绑定
- **WHEN** 服务端以默认配置启动
- **THEN** 仅监听 loopback，外部网络不可直连

### Requirement: 认证范围豁免
系统 MAY 豁免仅用于前端静态资源与登录页的访问；所有数据 API 与终端连接 MUST NOT 豁免。

#### Scenario: 静态资源可访问
- **WHEN** 浏览器请求前端静态资源
- **THEN** 无需 token；但随后的 API 请求必须带 token

### Requirement: 复杂认证非目标
本版本 SHALL NOT 实现多用户、角色权限、OAuth 等复杂认证机制。

#### Scenario: 单用户语义
- **WHEN** 任意持 token 者访问
- **THEN** 其拥有全部功能权限，无用户隔离概念