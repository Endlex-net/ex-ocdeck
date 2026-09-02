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

- **操作系统**：macOS 或 Linux（含 WSL2）
- **依赖**：`tmux`、`git`、`opencode` CLI

请确保上述命令在 `PATH` 中可用。Linux 下前两者可用包管理器安装：

```bash
sudo apt-get install -y tmux git
```

## 安装

### macOS（Homebrew）

正式安装渠道是 Homebrew：

```bash
brew tap Endlex-net/tap
brew install ocdeck
```

### Linux / WSL

一键安装（确定最新 Release → 下载并校验 → 安装到 `~/.local/bin` → 生成 env 文件 → 尝试配置 systemd 自启）：

```bash
curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash
```

升级到最新版（已安装版本与目标一致时跳过下载；服务运行中会自动 restart）：

```bash
curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash -s -- upgrade
```

两种子命令都可用 `OCDECK_VERSION=vX.Y.Z` 环境变量（或 `bash -s -- install --version vX.Y.Z` 参数）指定版本，可升可降。

以下手动步骤作为替代方案：

从 [Releases](https://github.com/Endlex-net/ex-ocdeck/releases) 下载对应架构的压缩包：`x86_64` 主机选 `ocdeck_linux_amd64.tar.gz`，`aarch64` 主机选 `ocdeck_linux_arm64.tar.gz`（可用 `uname -m` 确认架构）。

```bash
mkdir -p ~/.local/bin
tar -xzf ocdeck_linux_amd64.tar.gz -C ~/.local/bin ocdeck-server
chmod +x ~/.local/bin/ocdeck-server
```

确认 `~/.local/bin` 在 `PATH` 中（Ubuntu 默认会在该目录存在时自动加入，重新登录后生效）。

配置与 macOS 相同：编辑 `~/.config/ocdeck/env`，格式为 `KEY=VALUE`，与 systemd 的 `EnvironmentFile` 兼容；可用变量见[配置](#配置)章节，此处不重复。

#### 开机自启（systemd）

仓库提供 user service 模板 `contrib/ocdeck.service`（未 clone 仓库时，把该文件内容保存为 `~/.config/systemd/user/ocdeck.service` 即可）：

```bash
mkdir -p ~/.config/systemd/user
cp contrib/ocdeck.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now ocdeck
```

查看日志：

```bash
journalctl --user -u ocdeck -f
```

若服务启动报找不到 `opencode` / `tmux`（如 `executable file not found in $PATH`）：systemd 服务环境的 `PATH` 只有系统目录，而这类工具常装在用户目录（如 `~/.opencode/bin`）。确认 `~/.config/ocdeck/env` 的 `PATH=` 行包含其所在目录（完整字面值，不支持 `$PATH` 展开），修改后执行 `systemctl --user restart ocdeck`。`install-linux.sh` 生成的 env 文件会自动写入该行。

#### WSL2

- WSL2 需启用 systemd 后才能用上述自启方式：在 `/etc/wsl.conf` 写入

  ```ini
  [boot]
  systemd=true
  ```

  然后在 PowerShell 执行 `wsl --shutdown`，重新打开 WSL 生效。

- Windows 浏览器可直接访问 `http://127.0.0.1:<port>`，WSL2 的 localhost 转发默认开启。
- 项目和数据放在 WSL 文件系统（`~/`）下，不要放 `/mnt/c`：跨文件系统 IO 慢一个量级。
- 不启用 systemd 时，可手动运行 `~/.local/bin/ocdeck-server`，或用 Windows 任务计划程序在登录时调 `wsl` 命令拉起。

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

Linux 通过上文 systemd 命令启动（`systemctl --user start ocdeck`），日志用 `journalctl --user -u ocdeck -f` 查看。

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

Linux 下重新下载新版压缩包，按上文方式覆盖 `~/.local/bin/ocdeck-server` 后执行 `systemctl --user restart ocdeck`。

### 卸载

```bash
brew services stop ocdeck
brew uninstall ocdeck
```

Linux：`systemctl --user disable --now ocdeck`，再删除 `~/.config/systemd/user/ocdeck.service` 与 `~/.local/bin/ocdeck-server`。

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
