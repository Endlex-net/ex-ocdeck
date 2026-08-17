#!/usr/bin/env bash
# 对比两个 opencode ref 的契约锚点文件。
# 用法: scripts/check-opencode-contract.sh <oldRef> <newRef>
# 例:   scripts/check-opencode-contract.sh v1.18.18 v1.18.19
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <oldRef> <newRef>" >&2
  echo "  e.g. $0 v1.18.18 v1.18.19" >&2
  exit 2
fi

OLD_REF=$1
NEW_REF=$2
REPO=anomalyco/opencode

ANCHORS=(
  packages/schema/src/v1/permission.ts
  packages/schema/src/v1/question.ts
  packages/schema/src/v1/session.ts
  packages/schema/src/v1/legacy-event.ts
  packages/schema/src/session-status-event.ts
  packages/schema/src/session-event.ts
  packages/core/src/event.ts
  packages/opencode/src/server/routes/instance/httpapi/handlers/session.ts
  packages/opencode/src/server/routes/instance/httpapi/handlers/event.ts
  packages/opencode/src/server/routes/instance/httpapi/handlers/permission.ts
  packages/opencode/src/server/routes/instance/httpapi/handlers/question.ts
  packages/opencode/src/server/routes/instance/httpapi/handlers/global.ts
  packages/opencode/src/server/routes/instance/httpapi/groups/session.ts
  packages/opencode/src/server/routes/instance/httpapi/groups/event.ts
  packages/opencode/src/server/routes/instance/httpapi/groups/permission.ts
  packages/opencode/src/server/routes/instance/httpapi/groups/question.ts
  packages/opencode/src/server/routes/instance/httpapi/groups/global.ts
  packages/opencode/src/session/status.ts
  packages/opencode/src/event-v2-bridge.ts
  packages/opencode/src/cli/cmd/attach.ts
  packages/opencode/src/cli/tui/validate-session.ts
)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fetch() {
  local ref=$1 path=$2 dest=$3
  local url="https://raw.githubusercontent.com/${REPO}/${ref}/${path}"
  local attempt
  for attempt in 1 2 3; do
    if curl -fsSL --max-time 60 "$url" -o "$dest" && [[ -s "$dest" ]]; then
      return 0
    fi
    rm -f "$dest"
    sleep 1
  done
  return 1
}

failed=0

echo "==> comparing ${#ANCHORS[@]} anchors: ${OLD_REF} → ${NEW_REF}"
for path in "${ANCHORS[@]}"; do
  old_file="${TMP}/old/${path}"
  new_file="${TMP}/new/${path}"
  mkdir -p "$(dirname "$old_file")" "$(dirname "$new_file")"

  if ! fetch "$OLD_REF" "$path" "$old_file"; then
    echo "FETCH FAIL ${OLD_REF} ${path}" >&2
    failed=1
    continue
  fi
  if ! fetch "$NEW_REF" "$path" "$new_file"; then
    echo "FETCH FAIL ${NEW_REF} ${path}" >&2
    failed=1
    continue
  fi

  if diff -q "$old_file" "$new_file" >/dev/null; then
    echo "SAME ${path}"
  else
    echo "DIFF ${path}"
    diff -u "$old_file" "$new_file" || true
    failed=1
  fi
done

echo
echo "==> live-probe checklist（手工启动后执行）"
echo "    opencode serve --port <p> --hostname 127.0.0.1"
echo "    export OPENCODE_SERVER_PASSWORD=<password>"
cat <<'EOF'
    AUTH=(-u "opencode:${OPENCODE_SERVER_PASSWORD}")
    BASE=http://127.0.0.1:<p>
    DIR=$(python3 -c 'import urllib.parse,os; print(urllib.parse.quote(os.getcwd(), safe=""))')
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/global/health"
    SID=$(curl -sS --fail-with-body "${AUTH[@]}" -X POST "${BASE}/session?directory=${DIR}" \
      -H 'Content-Type: application/json' -d '{"title":"contract-probe"}' \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session/${SID}?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session?directory=${DIR}&limit=1"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session/status?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/permission?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/question?directory=${DIR}"
    # 短采样：首事件应为 server.connected
    curl -sS --max-time 2 "${AUTH[@]}" "${BASE}/event?directory=${DIR}" || true
    # 手工 CLI 检查（不要自动化 TUI）：opencode attach http://127.0.0.1:<p> --session $SID
    curl -sS --fail-with-body "${AUTH[@]}" -X DELETE "${BASE}/session/${SID}?directory=${DIR}"
EOF

if [[ "$failed" -ne 0 ]]; then
  echo "==> FAILED: diffs or fetch errors above" >&2
  exit 1
fi
echo "==> OK: all anchors identical"
