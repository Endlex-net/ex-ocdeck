# ocdeck

自托管的 opencode 任务编排 Web 控制台。Go 后端嵌入 Web 前端，以单二进制运行。

[![CI](https://github.com/Endlex-net/ex-ocdeck/actions/badge.svg)](https://github.com/Endlex-net/ex-ocdeck/actions)
[![Release](https://img.shields.io/github/v/release/Endlex-net/ex-ocdeck)](https://github.com/Endlex-net/ex-ocdeck/releases)

## 功能特性

- 在浏览器里编排、查看和管理 opencode 任务
- Web 终端接入任务会话
- 任务状态与数据持久化到本地目录
- 任务相关通知
- 单二进制部署：前端资源嵌入服务端，无需单独起静态站点

v1 面向本机自托管，默认只监听本机地址。

## 系统要求

- **操作系统**：macOS（v1 仅支持 macOS）
- **依赖**：`tmux`、`git`、`opencode` CLI

请确保上述命令在 `PATH` 中可用。

## 安装

正式安装渠道是 Homebrew：

```bash
brew tap Endlex-net/tap
brew install ocdeck
```

## 配置

配置文件默认路径：`~/.config/ocdeck/env`，格式为 `KEY=VALUE` 行。

server 启动时读取该文件并注入进程环境：

- `PATH`：**无条件覆盖**
- 其它键：**真实环境优先**（进程里已有的值不会被文件覆盖）

可用 `OCDECK_ENV_FILE` 覆盖 env 文件路径。

| 变量 | 必填 | 说明 |
|------|------|------|
| `OCDECK_TOKEN` | 是 | 访问令牌，浏览器与 API 鉴权使用 |
| `OCDECK_LISTEN_ADDR` | 否 | 监听地址，默认 `127.0.0.1` |
| `OCDECK_LISTEN_PORT` | 否 | 固定监听端口；与 `OCDECK_SERVE_PORT_RANGE` **二选一** |
| `OCDECK_SERVE_PORT_RANGE` | 否 | 自动选端口范围，格式 `MIN-MAX`；与 `OCDECK_LISTEN_PORT` **二选一** |
| `OCDECK_DATA_DIR` | 否 | 数据目录，默认 `~/.ocdeck` |
| `OCDECK_SHUTDOWN_POLICY` | 否 | 关停策略：`persist` / `kill_on_start` / `kill_immediate` |
| `OCDECK_ALLOWED_ORIGINS` | 否 | Web 来源白名单，逗号分隔 |
| `OCDECK_ENV_FILE` | 否 | 覆盖默认 env 文件路径 |

最小配置示例：

```bash
cat > ~/.config/ocdeck/env <<'EOF'
OCDECK_TOKEN=replace-me
OCDECK_LISTEN_PORT=8787
EOF
```

请把 `OCDECK_TOKEN` 换成足够随机的私密值，不要提交到仓库或分享给他人。

## 使用

### 启动

```bash
brew services start ocdeck
```

`brew services` 会开机自启，并在进程崩溃后由 launchd 拉起。

日志默认写到：

```text
/opt/homebrew/var/log/ocdeck.log
```

### 访问

在浏览器打开：

```text
http://127.0.0.1:<port>
```

`<port>` 为 `OCDECK_LISTEN_PORT`，或由 `OCDECK_SERVE_PORT_RANGE` 自动选出的端口。使用 `OCDECK_TOKEN` 鉴权。

### 升级

```bash
brew upgrade ocdeck
```

### 卸载

```bash
brew services stop ocdeck
brew uninstall ocdeck
```

## 从源码构建

需要本机已安装 Go、Node.js / pnpm，以及上文系统要求中的依赖。

```bash
cd web
pnpm install
pnpm build
cd ..
go build ./cmd/ocdeck-server
```

前端构建产物会 embed 进二进制。查看版本：

```bash
./ocdeck-server --version
```

## License

MIT
