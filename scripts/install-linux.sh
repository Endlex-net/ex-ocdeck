#!/usr/bin/env bash
# ocdeck Linux/WSL 安装/升级脚本，支持 curl 管道执行：
#   curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash -s -- upgrade
#
# 流程：确定目标版本 → 下载 Release tarball 并做 sha256 校验 → 安装 ocdeck-server
# 到 ~/.local/bin → 首次安装时生成 ~/.config/ocdeck/env（已存在则沿用）→ 尝试配置
# systemd user service 自启（WSL 未开 systemd 时打印手动指引）。
set -euo pipefail

REPO="Endlex-net/ex-ocdeck"
BASE_URL="https://github.com/${REPO}"
BIN_DIR="$HOME/.local/bin"
ENV_FILE="$HOME/.config/ocdeck/env"
UNIT_FILE="$HOME/.config/systemd/user/ocdeck.service"

info() { echo "[ocdeck] $*"; }
warn() { echo "[ocdeck] 警告: $*" >&2; }
die()  { echo "[ocdeck] 错误: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法: install-linux.sh [install|upgrade] [--version vX.Y.Z]

  install   默认子命令：安装指定版本（默认最新 Release），覆盖已有二进制
  upgrade   升级到指定版本（默认最新）；与已安装版本一致时跳过下载
  --version 指定版本，v 前缀可选；也可用环境变量 OCDECK_VERSION（--version 优先）

示例:
  curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash
  curl -fsSL https://raw.githubusercontent.com/Endlex-net/ex-ocdeck/main/scripts/install-linux.sh | bash -s -- upgrade
  OCDECK_VERSION=v0.3.0 bash scripts/install-linux.sh
EOF
}

# ---------- 参数 ----------

MODE="install"
VERSION_ARG=""
while [ $# -gt 0 ]; do
  case "$1" in
    install|upgrade)
      MODE="$1"; shift ;;
    --version)
      [ $# -ge 2 ] || die "--version 缺少参数值"
      VERSION_ARG="$2"; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      die "未知参数: $1（-h 查看用法）" ;;
  esac
done
# --version 优先于 OCDECK_VERSION，统一去掉 v 前缀
VERSION_ARG="${VERSION_ARG:-${OCDECK_VERSION:-}}"
VERSION_ARG="${VERSION_ARG#v}"

# ---------- 平台与依赖 ----------

[ "$(uname -s)" = "Linux" ] || die "本脚本仅支持 Linux（当前: $(uname -s)）；macOS 请用 brew install ocdeck"

case "$(uname -m)" in
  x86_64)        ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             die "不支持的架构: $(uname -m)（Release 仅提供 linux amd64/arm64）" ;;
esac

missing=()
for cmd in tmux git; do
  command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
done
if [ "${#missing[@]}" -gt 0 ]; then
  if command -v apt-get >/dev/null 2>&1 && command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    info "安装缺失依赖: ${missing[*]}（apt-get）"
    sudo apt-get update -qq
    sudo apt-get install -y -qq "${missing[@]}"
  else
    die "缺少依赖: ${missing[*]}。请手动安装后重试（Debian/Ubuntu: sudo apt-get install -y ${missing[*]}；其他发行版用对应包管理器）"
  fi
fi
if ! command -v opencode >/dev/null 2>&1; then
  warn "未检测到 opencode CLI，任务编排功能需要它；请稍后自行安装 opencode 后再使用（不影响本次安装）"
fi

# ---------- 版本 ----------

latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | sed 's/^v//'
}

if [ -n "$VERSION_ARG" ]; then
  TARGET_VERSION="$VERSION_ARG"
else
  info "查询最新 Release 版本 ..."
  TARGET_VERSION="$(latest_version)"
  [ -n "$TARGET_VERSION" ] || die "无法获取最新版本号（api.github.com 不可达或限流）；可用 OCDECK_VERSION=vX.Y.Z 指定版本后重试"
fi

# 已安装版本：release 二进制 --version 输出单行版本号（无 v 前缀）
installed_version=""
if [ -x "$BIN_DIR/ocdeck-server" ]; then
  installed_version="$("$BIN_DIR/ocdeck-server" --version 2>/dev/null | head -n1 || true)"
  installed_version="${installed_version#v}"
fi

if [ "$MODE" = "upgrade" ]; then
  if [ -z "$installed_version" ] || [ "$installed_version" = "dev" ]; then
    warn "未检测到已安装的 ocdeck-server（或版本无法识别），按 install 处理"
  elif [ "$installed_version" = "$TARGET_VERSION" ]; then
    info "已安装 ${installed_version}，目标 ${TARGET_VERSION}，无需升级"
    exit 0
  else
    info "升级 ${installed_version} → ${TARGET_VERSION}"
  fi
fi

# ---------- 下载与校验 ----------

TAG="v${TARGET_VERSION}"
TARBALL="ocdeck_linux_${ARCH}.tar.gz"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

info "下载 ${TAG}/${TARBALL} 与 checksums.txt ..."
curl -fsSL -o "$tmpdir/$TARBALL"      "${BASE_URL}/releases/download/${TAG}/${TARBALL}"
curl -fsSL -o "$tmpdir/checksums.txt" "${BASE_URL}/releases/download/${TAG}/checksums.txt"

expected="$(awk -v f="$TARBALL" '$2 == f { print $1 }' "$tmpdir/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt 中没有 ${TARBALL} 的条目"
actual="$(sha256sum "$tmpdir/$TARBALL" | awk '{ print $1 }')"
[ "$actual" = "$expected" ] || die "sha256 校验失败: 期望 ${expected}，实际 ${actual}"

# ---------- 安装 ----------

# 升级场景：enable --now 不会重启已 active 的服务，替换二进制后需显式 restart
was_active=0
if command -v systemctl >/dev/null 2>&1 && systemctl --user is-active --quiet ocdeck 2>/dev/null; then
  was_active=1
fi

mkdir -p "$BIN_DIR"
# tarball 为平铺结构（ocdeck-server + README.md），只解出二进制成员
tar -xzf "$tmpdir/$TARBALL" -C "$tmpdir" ocdeck-server
install -m 0755 "$tmpdir/ocdeck-server" "$BIN_DIR/ocdeck-server"
info "已安装 ${BIN_DIR}/ocdeck-server（${TAG}）"

# ---------- PATH ----------

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    # 幂等：~/.bashrc 已有同样一行则不重复追加
    if ! grep -qsF 'export PATH="$HOME/.local/bin:$PATH"' "$HOME/.bashrc"; then
      printf '\n# added by ocdeck install-linux.sh\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$HOME/.bashrc"
    fi
    warn "${BIN_DIR} 不在 PATH，已追加到 ~/.bashrc；请重开 shell 或执行 source ~/.bashrc 生效"
    ;;
esac

# ---------- env 文件 ----------

# 服务环境 PATH：以当前 shell 的 PATH 为基础，确保含 opencode 所在目录与 ~/.local/bin。
# env 文件按字面值读取（config.parseEnvFile 不做 $ 展开），必须写入完整路径；
# server 启动时用该行无条件覆盖服务进程 PATH（config.ApplyEnvFile），从而让
# systemd 服务（默认 PATH 只有系统目录）也能找到 opencode/tmux 等用户目录下的工具。
service_path="$PATH"
if command -v opencode >/dev/null 2>&1; then
  opencode_dir="$(dirname "$(command -v opencode)")"
  case ":$service_path:" in
    *":$opencode_dir:"*) ;;
    *) service_path="${opencode_dir}:${service_path}" ;;
  esac
fi
case ":$service_path:" in
  *":$BIN_DIR:"*) ;;
  *) service_path="${BIN_DIR}:${service_path}" ;;
esac

mkdir -p "$(dirname "$ENV_FILE")"
if [ -f "$ENV_FILE" ]; then
  info "env 文件已存在，沿用 ${ENV_FILE}"
  if grep -q '^PATH=' "$ENV_FILE"; then
    # 已有 PATH= 行视为用户显式配置，不改动
    info "已存在 PATH= 行，保持不动（用户显式配置优先）"
  else
    printf '\n# 服务环境 PATH（install-linux.sh 追加；完整字面值，不支持 $ 展开）\nPATH=%s\n' "$service_path" >> "$ENV_FILE"
    info "已追加 PATH= 行（systemd 服务默认 PATH 不含 ~/.opencode/bin 等用户目录，服务据此行定位 opencode/tmux）"
  fi
else
  # 随机 token：/dev/urandom + tr/cut，不依赖 openssl（LC_ALL=C 保证按字节处理）
  token="$(head -c 256 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c 1-32)"
  ( umask 077
    cat > "$ENV_FILE" <<EOF
# ocdeck 配置文件（KEY=VALUE，# 开头为注释），全部变量见 README「配置」章节
OCDECK_TOKEN=${token}
# 服务环境 PATH（完整字面值，不支持 \$ 展开）；systemd 服务据此定位 opencode/tmux 等工具
PATH=${service_path}
# 固定监听端口（默认 7474）；注释掉则改由 OCDECK_SERVE_PORT_RANGE 自动探测（默认 50000-50999）
OCDECK_LISTEN_PORT=7474
EOF
  )
  info "已生成 ${ENV_FILE}（权限 0600），随机 OCDECK_TOKEN 与 PATH 行已写入"
fi

# ---------- systemd ----------

service_mode="manual"
if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  mkdir -p "$(dirname "$UNIT_FILE")"
  # 内容与仓库 contrib/ocdeck.service 保持一致；curl 单文件执行，不引用仓库文件
  cat > "$UNIT_FILE" <<'EOF'
[Unit]
Description=ocdeck server
After=network.target

[Service]
ExecStart=%h/.local/bin/ocdeck-server
EnvironmentFile=%h/.config/ocdeck/env
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now ocdeck
  if [ "$was_active" = "1" ]; then
    systemctl --user restart ocdeck
    info "检测到 ocdeck 服务已在运行，已重启加载新版本"
  fi
  service_mode="systemd"
else
  warn "systemctl --user 不可用，跳过自启配置"
  if grep -qi microsoft /proc/version 2>/dev/null; then
    cat >&2 <<'EOF'
[ocdeck] 检测到 WSL。如需 systemd 自启：
  1. 在 /etc/wsl.conf 写入：
       [boot]
       systemd=true
  2. 在 PowerShell 执行 wsl --shutdown，重新打开 WSL
  3. 重新运行本脚本完成自启配置
EOF
  fi
fi

# ---------- 摘要 ----------

echo
info "${MODE} 完成：ocdeck ${TAG}（linux/${ARCH}）"
info "二进制：  ${BIN_DIR}/ocdeck-server"
if [ "$service_mode" = "systemd" ]; then
  info "服务：    systemctl --user status ocdeck（日志：journalctl --user -u ocdeck -f）"
else
  info "手动启动：${BIN_DIR}/ocdeck-server"
fi
info "访问：    http://127.0.0.1:7474（固定端口已默认启用；如需自动探测端口，注释 ${ENV_FILE} 中 OCDECK_LISTEN_PORT 行，由 OCDECK_SERVE_PORT_RANGE 50000-50999 探测）"
info "Token：   ${ENV_FILE} 中的 OCDECK_TOKEN"
info "PATH：    服务环境 PATH 取自 ${ENV_FILE} 的 PATH= 行（如服务报找不到命令，检查该行是否含其所在目录）"
info "卸载：    systemctl --user disable --now ocdeck; rm -f ${BIN_DIR}/ocdeck-server ${UNIT_FILE}（数据目录 ~/.ocdeck 保留，可自行删除）"
