#!/usr/bin/env bash
# docker/entrypoint-ocdeck.sh —— ocdeck-v2 手动测试镜像入口。
#
# 职责（契约）：
#   1. set -euo pipefail；OCDECK_TOKEN 必填（ocdeck-server 启动硬需求，
#      internal/config/config.go:474 探测——缺失即拒绝启动）。
#   2. git 全局身份兜底（ocdeck 生命周期需要 git；容器内无既有配置）。
#   3. 从 env 生成 opencode 模型配置 $HOME/.config/opencode/opencode.json
#      （XDG_CONFIG_HOME 设置时改用其下 opencode/）。apiKey 仅以
#      {env:ARCHI_API_KEY} 引用——任何文件/镜像层不得出现字面 token。
#      ARCHI_API_KEY 缺失：打印醒目警告但不失败（允许只测 UI/生命周期）。
#   4. 打印最终 listen 地址后 exec ocdeck-server。
set -euo pipefail

: "${OCDECK_TOKEN:?OCDECK_TOKEN is required (UI 密码；缺失即拒绝启动)}"
export OCDECK_LISTEN_ADDR="${OCDECK_LISTEN_ADDR:-0.0.0.0}"
export OCDECK_LISTEN_PORT="${OCDECK_LISTEN_PORT:-8788}"
export OCDECK_DATA_DIR="${OCDECK_DATA_DIR:-/data}"
export OCDECK_V2_ADOPTION="${OCDECK_V2_ADOPTION:-1}"

# git 兜底身份（提交/worktree 生命周期需要；容器内无既有全局配置）。
git config --global user.email "ocdeck@localhost"
git config --global user.name "ocdeck"

# opencode 模型配置（archi-openai-fast；与任务契约 JSON 逐字段一致）。
config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
mkdir -p "$config_dir"
cat > "$config_dir/opencode.json" <<'JSON'
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "archi-openai-fast": {
      "npm": "@ai-sdk/openai",
      "name": "Archi OpenAI Fast",
      "options": {"baseURL": "https://archi.marscore.net/v1", "apiKey": "{env:ARCHI_API_KEY}", "setCacheKey": true},
      "models": {"gpt-5.4": {}, "gpt-5.5": {}}
    }
  },
  "model": "archi-openai-fast/gpt-5.5",
  "small_model": "archi-openai-fast/gpt-5.4"
}
JSON

if [ -n "${ARCHI_API_KEY:-}" ]; then
	# ocdeck 托管的 v2 serve 经 tmux 白名单 env 启动（仅 OPENCODE_SERVER_PASSWORD
	# + DefaultBaseEnv），{env:ARCHI_API_KEY} 插值在 serve 进程内取不到值——
	# 测试镜像形态：生成时替换为字面 key（key 仅来自 docker run -e，不进镜像层）。
	sed -i "s|{env:ARCHI_API_KEY}|${ARCHI_API_KEY}|" "$config_dir/opencode.json"
else
	{
		echo "====================================================================="
		echo "WARNING: ARCHI_API_KEY 未设置——opencode 模型调用不可用。"
		echo "         仍可手动测试 UI / 任务生命周期（不触发模型请求）。"
		echo "====================================================================="
	} >&2
fi

echo "ocdeck-server listening on ${OCDECK_LISTEN_ADDR}:${OCDECK_LISTEN_PORT} (data dir: ${OCDECK_DATA_DIR}, v2 adoption: ${OCDECK_V2_ADOPTION})"
exec /usr/local/bin/ocdeck-server
