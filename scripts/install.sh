#!/bin/zsh
# ocdeck 更新安装脚本：构建前端 + 后端二进制，安装到 ~/.local/bin/ocdeck-server
# 用法: ./scripts/install.sh [--restart]
#   --restart  安装后重启正在运行的 ocdeck-server（persist 模式下任务进程保留，自动恢复）
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$HOME/.local/bin/ocdeck-server"

echo "==> 构建前端 (web/)"
(cd "$REPO/web" && pnpm install --frozen-lockfile --silent && pnpm build)

echo "==> 构建 Go 二进制"
TMP_BIN="$(mktemp -t ocdeck-server.XXXXXX)"
trap 'rm -f "$TMP_BIN"' EXIT
(cd "$REPO" && go build -trimpath -ldflags="-s -w" -o "$TMP_BIN" ./cmd/ocdeck-server)

echo "==> 安装到 $TARGET"
mkdir -p "$(dirname "$TARGET")"
install -m 0755 "$TMP_BIN" "$TARGET"
echo "    installed: $(ls -lh "$TARGET" | awk '{print $5}') $TARGET"

if [[ "${1:-}" == "--restart" ]]; then
  if pgrep -f "ocdeck-server" >/dev/null 2>&1; then
    echo "==> 重启运行中的 ocdeck-server"
    pkill -f "ocdeck-server" || true
    sleep 2
    echo "    已停止旧实例（persist 模式：tmux 会话保留，重启后自动恢复）"
    echo "    请按你常用的环境变量重新启动，例如："
    echo "    OCDECK_TOKEN=<token> $TARGET"
  else
    echo "==> 无运行中实例，跳过重启"
  fi
else
  echo "完成。如需重启运行中的实例，重新执行: $0 --restart"
fi
