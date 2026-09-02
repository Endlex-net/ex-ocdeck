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
  packages/opencode/src/cli/cmd/tui.ts
  packages/opencode/src/cli/tui/worker.ts
  # diff-review-workbench 新增锚点：prompt_async 提交通道
  packages/opencode/src/session/prompt.ts
)

if [[ ${#ANCHORS[@]} -ne 24 ]]; then
  echo "expected 24 contract anchors, got ${#ANCHORS[@]}" >&2
  exit 2
fi

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
echo "==> live-probe checklist（手工执行；bare TUI external。先设密码，后台启动 + trap 清理）"
cat <<'EOF'
    set -euo pipefail
    export OPENCODE_SERVER_PASSWORD=<password>
    P=<p>
    P2=<p2>
    opencode --port "$P" --hostname 127.0.0.1 >/tmp/ocdeck-contract-p.log 2>&1 &
    pid=$!
    pid2=""
    trap 'kill $pid ${pid2:-} 2>/dev/null || true' EXIT
    AUTH=(-u "opencode:${OPENCODE_SERVER_PASSWORD}")
    BASE=http://127.0.0.1:${P}
    DIR=$(python3 -c 'import urllib.parse,os; print(urllib.parse.quote(os.getcwd(), safe=""))')
    ready=0
    for i in $(seq 1 50); do
      if curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${BASE}/global/health" | grep -q '^200$'; then
        ready=1
        break
      fi
      sleep 0.1
    done
    [[ "$ready" == "1" ]]
    code=$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/global/health")
    [[ "$code" == "401" ]]
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/global/health"
    SID=$(curl -sS --fail-with-body "${AUTH[@]}" -X POST "${BASE}/session?directory=${DIR}" \
      -H 'Content-Type: application/json' -d '{"title":"contract-probe"}' \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session/${SID}?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session?directory=${DIR}&limit=1"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/session/status?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/permission?directory=${DIR}"
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/question?directory=${DIR}"
    ev=$(curl -sS --max-time 2 "${AUTH[@]}" "${BASE}/event?directory=${DIR}" || true)
    echo "$ev" | grep -q 'server.connected'
    # 必须用 ses_ 前缀：SessionID schema 要求 startsWith("ses")，否则只会走
    # 本地 decode 的 "Invalid session ID"，测不到 HTTP 404 / Session not found。
    if ! grep -q "Error: Session not found" <(opencode --port "$P2" --hostname 127.0.0.1 --session ses_doesnotexist00000000000000 2>&1 || true); then
      echo "FAIL: missing --session must reject with 'Error: Session not found'" >&2
      exit 1
    fi
    opencode --port "$P2" --hostname 127.0.0.1 --session "$SID" >/tmp/ocdeck-contract-p2.log 2>&1 &
    pid2=$!
    BASE2=http://127.0.0.1:${P2}
    ready2=0
    for i in $(seq 1 50); do
      if curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" "${BASE2}/global/health" | grep -q '^200$'; then
        ready2=1
        break
      fi
      sleep 0.1
    done
    [[ "$ready2" == "1" ]]
    curl -sS --fail-with-body "${AUTH[@]}" "${BASE2}/session?directory=${DIR}&limit=20" | grep -q "$SID"
    curl -sS --fail-with-body "${AUTH[@]}" -X DELETE "${BASE}/session/${SID}?directory=${DIR}"
    # diff-review-workbench：/doc 能力探测 + prompt_async 端点形状（精确断言）
    # /doc 无 Auth（已配置密码）→ 401
    code=$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/doc")
    [[ "$code" == "401" ]]
    DOC=$(curl -sS --fail-with-body "${AUTH[@]}" "${BASE}/doc")
    echo "$DOC" | python3 -c '
import json, sys
d = json.load(sys.stdin)
p = d["paths"]["/session/{sessionID}/prompt_async"]
assert p["post"]["operationId"] == "session.prompt_async", "operationId mismatch"
# sessionID 路径参数 pattern ^ses（Schema.isStartsWith("ses") → ^ses）
sid_param = next(x for x in p["post"].get("parameters", []) if x.get("name") == "sessionID")
assert sid_param["schema"]["pattern"] == "^ses", f"sessionID pattern: {sid_param['schema'].get('pattern')}"
# messageID requestBody schema pattern ^msg（Schema.isStartsWith("msg") → ^msg）
req_schema = p["post"]["requestBody"]["content"]["application/json"]["schema"]
mid = req_schema["properties"]["messageID"]
assert mid["pattern"] == "^msg", f"messageID pattern: {mid.get('pattern')}"
print("doc prompt_async ok (opId, ^ses, ^msg)")
'
    # 不满足 sessionID pattern 的 id（如 missing-session，无 ses 前缀）→ 500（参数校验异常）
    code=$(curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" -X POST "${BASE}/session/missing-session/prompt_async?directory=${DIR}" -H 'Content-Type: application/json' -d '{"messageID":"msg_test","parts":[{"type":"text","text":"probe"}]}')
    [[ "$code" == "500" ]]
    # 格式合法但不存在 session id（如 ses_does_not_exist，含 ses 前缀）→ 404
    code=$(curl -sS -o /dev/null -w '%{http_code}' "${AUTH[@]}" -X POST "${BASE}/session/ses_does_not_exist/prompt_async?directory=${DIR}" -H 'Content-Type: application/json' -d '{"messageID":"msg_test","parts":[{"type":"text","text":"probe"}]}')
    [[ "$code" == "404" ]]
    echo "doc + prompt_async shape ok"
EOF

if [[ "$failed" -ne 0 ]]; then
  echo "==> FAILED: diffs or fetch errors above" >&2
  exit 1
fi
echo "==> OK: all anchors identical"
