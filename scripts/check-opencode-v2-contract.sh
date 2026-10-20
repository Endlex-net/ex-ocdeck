#!/usr/bin/env bash
# opencode v2 (2.0.28) 契约锚点核验：对锁定源码锚点 commit 拉取锚点文件，
# 做存在性 + 形态断言（端点路径 / envelope / 事件名 / schema 字段名）。
# 断言语义=静态锚点层级（源码漂移检测）；运行时 / live 语义不由本脚本覆盖，
# 由 evidence run 验证（R1=20261015T165420Z-4013391d 无模型 run、
# R2=20261016T090000Z-0bb6ac6c 模型依赖 run），事实清单见 CONTRACT.md。
# 契约事实与升级 SOP：internal/infrastructure/opencode2/CONTRACT.md
# 用法: scripts/check-opencode-v2-contract.sh
set -euo pipefail

COMMIT=026b106bc5d41b679ffc413264229c340871e4d3 # tag v2.0.28（源码锚点；二进制构建 commit 044d8b7d 分别记录于 CONTRACT.md）
REPO=anomalyco/opencode

ANCHORS=(
  packages/protocol/src/groups/session.ts
  packages/protocol/src/groups/permission.ts
  packages/protocol/src/groups/event.ts
  packages/protocol/src/groups/form.ts
  packages/protocol/src/api.ts
  packages/schema/src/session-id.ts
  packages/schema/src/session-message.ts
  packages/schema/src/event.ts
  packages/schema/src/permission.ts
  packages/schema/src/prompt-input.ts
  packages/schema/src/form.ts
  packages/schema/src/session-event.ts
  packages/schema/src/session.ts
  packages/cli/src/commands/commands.ts
  packages/cli/src/commands/handlers/default.ts
  packages/protocol/src/errors.ts
  packages/server/src/routes.ts
  packages/server/src/middleware/authorization.ts
)

if [[ ${#ANCHORS[@]} -ne 18 ]]; then
  echo "expected 18 contract anchors, got ${#ANCHORS[@]}" >&2
  exit 2
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fetch() {
  local path=$1 dest=$2
  local url="https://raw.githubusercontent.com/${REPO}/${COMMIT}/${path}"
  local attempt
  for attempt in 1 2 3; do
    if curl -fsSL --max-time 90 "$url" -o "$dest" && [[ -s "$dest" ]]; then
      return 0
    fi
    rm -f "$dest"
    sleep 1
  done
  return 1
}

failed=0

echo "==> fetching ${#ANCHORS[@]} anchors at ${COMMIT:0:8} (v2.0.28)"
for path in "${ANCHORS[@]}"; do
  dest="${TMP}/${path}"
  mkdir -p "$(dirname "$dest")"
  if ! fetch "$path" "$dest"; then
    echo "FETCH FAIL ${path}" >&2
    failed=1
  fi
done

if [[ "$failed" -ne 0 ]]; then
  echo "==> FAILED: fetch errors above" >&2
  exit 1
fi

# 形态断言（锚点文件|逐字片段）。片段与 CONTRACT.md 冻结事实一一对应。
ASSERTS=(
  # session 组：端点路径 / envelope / 光标分页（CONTRACT.md §2 §3 §4 §6）
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.get("session.list", "/api/session"'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.post("session.create", "/api/session"'
  'packages/protocol/src/groups/session.ts|location: Location.PublicRef.pipe(Schema.optional),'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.patch("session.update", "/api/session/:sessionID"'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.delete("session.remove", "/api/session/:sessionID"'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.post("session.prompt", "/api/session/:sessionID/prompt"'
  'packages/protocol/src/groups/session.ts|id: SessionMessage.ID.pipe(Schema.optional),'
  'packages/protocol/src/groups/session.ts|success: Schema.Struct({ data: SessionInbox.User })'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.post("session.interrupt", "/api/session/:sessionID/interrupt"'
  'packages/protocol/src/groups/session.ts|interrupted: Schema.Boolean'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.put("session.environment", "/api/session/:sessionID/environment"'
  'packages/protocol/src/groups/session.ts|variables: Schema.Record(Schema.String, Schema.String)'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.get("session.form.list", "/api/session/:sessionID/form"'
  'packages/protocol/src/groups/session.ts|HttpApiEndpoint.post("session.form.reply", "/api/session/:sessionID/form/:formID/reply"'
  'packages/protocol/src/groups/session.ts|FormAlreadySettledError'
  'packages/protocol/src/groups/session.ts|previous: SessionsCursor.pipe(Schema.optional),'
  'packages/protocol/src/groups/session.ts|next: SessionsCursor.pipe(Schema.optional),'
  # permission 组：回复端点 / saved 语义（CONTRACT.md §5）
  'packages/protocol/src/groups/permission.ts|HttpApiEndpoint.get("permission.request.list", "/api/permission/request"'
  'packages/protocol/src/groups/permission.ts|HttpApiEndpoint.get("permission.saved.list", "/api/permission/saved"'
  'packages/protocol/src/groups/permission.ts|success: Schema.Struct({ data: Schema.Array(PermissionSaved.Info) })'
  'packages/protocol/src/groups/permission.ts|HttpApiEndpoint.get("session.permission.list", "/api/session/:sessionID/permission"'
  'packages/protocol/src/groups/permission.ts|HttpApiEndpoint.post("session.permission.reply", "/api/session/:sessionID/permission/:requestID/reply"'
  'packages/protocol/src/groups/permission.ts|decision: Permission.Reply,'
  # event 组：SSE 订阅（CONTRACT.md §7）
  'packages/protocol/src/groups/event.ts|HttpApiEndpoint.get("event.subscribe", "/api/event"'
  'packages/protocol/src/groups/event.ts|HttpApiSchema.StreamSse({ data: EventSchema })'
  # form 组：全局 pending form 列表（CONTRACT.md §6）
  'packages/protocol/src/groups/form.ts|HttpApiEndpoint.get("form.list", "/api/form"'
  # ID 前缀（CONTRACT.md §2 §3 §5 §6 §7）
  'packages/schema/src/session-id.ts|isStartingWith("ses")'
  'packages/schema/src/session-message.ts|isStartingWith("msg_")'
  'packages/schema/src/event.ts|isStartingWith("evt_")'
  'packages/schema/src/event.ts|aggregateID: Schema.String, seq: Seq, version: Version'
  'packages/schema/src/permission.ts|isStartingWith("per")'
  'packages/schema/src/permission.ts|type: "permission.asked"'
  'packages/schema/src/form.ts|isStartingWith("frm_")'
  # prompt 附件 schema：无 mime 字段（CONTRACT.md §3）
  'packages/schema/src/prompt-input.ts|identifier: "PromptInput.FileAttachment"'
  'packages/schema/src/prompt-input.ts|uri: Schema.String,'
  # 事件名与归属键（CONTRACT.md §7 §8；G1-F3）
  'packages/schema/src/session-event.ts|type: "session.execution.started"'
  'packages/schema/src/session-event.ts|type: "session.execution.succeeded"'
  'packages/schema/src/session-event.ts|type: "session.deleted"'
  'packages/schema/src/session-event.ts|type: "session.renamed",'
  'packages/schema/src/session-event.ts|const Base = {'
  'packages/schema/src/session-event.ts|sessionID: SessionID,'
  'packages/schema/src/session-event.ts|aggregate: "sessionID",'
  # server.connected 首帧事件（CONTRACT.md §7；G1-F3）
  'packages/protocol/src/groups/event.ts|type: Schema.Literal("server.connected"),'
  'packages/protocol/src/groups/event.ts|identifier: "V2Event.server.connected"'
  # OpenAPI 契约发现端点（CONTRACT.md §1；G1-F3）
  'packages/server/src/routes.ts|openapiPath: "/openapi.json"'
  # 错误分类学：form 族 404/409/400、permission 幽灵 404、分页 400（CONTRACT.md §2 §5 §6；G1-F3）
  'packages/protocol/src/errors.ts|export class FormNotFoundError extends Schema.TaggedError<FormNotFoundError>()('
  'packages/protocol/src/errors.ts|export class FormAlreadySettledError extends Schema.TaggedError<FormAlreadySettledError>()('
  'packages/protocol/src/errors.ts|export class FormInvalidAnswerError extends Schema.TaggedError<FormInvalidAnswerError>()('
  'packages/protocol/src/errors.ts|export class PermissionNotFoundError extends Schema.TaggedError<PermissionNotFoundError>()('
  'packages/protocol/src/errors.ts|export class InvalidCursorError extends Schema.TaggedError<InvalidCursorError>()('
  'packages/protocol/src/groups/session.ts|error: [SessionNotFoundError, FormAlreadySettledError, FormInvalidAnswerError, FormNotFoundError],'
  # 就绪判定 401 语义（Basic challenge + UnauthorizedError；CONTRACT.md §1；G1-F3）
  "packages/server/src/middleware/authorization.ts|const WWW_AUTHENTICATE = 'Basic realm=\"Secure Area\"'"
  'packages/server/src/middleware/authorization.ts|const UNAUTHORIZED_MESSAGE = "Authentication required"'
  'packages/server/src/middleware/authorization.ts|{ status: 401, headers:'
  'packages/protocol/src/api.ts|.middleware(Authorization)'
  # outcome 仅诊断（CONTRACT.md §8）
  'packages/schema/src/session.ts|Schema.Literals(["succeeded", "failed", "interrupted"])'
  # CLI：--session 旗标与 TUI 自动选中（CONTRACT.md 升级 SOP / 1.4 结论）
  'packages/cli/src/commands/commands.ts|Flag.String("session")'
  'packages/cli/src/commands/commands.ts|Session ID to continue, or to create if it does not exist'
  'packages/cli/src/commands/commands.ts|Spec.make("serve"'
  'packages/cli/src/commands/handlers/default.ts|sessionID: sessionExists ? session : undefined,'
  'packages/cli/src/commands/handlers/default.ts|newSessionID: sessionExists ? undefined : session,'
)

if [[ ${#ASSERTS[@]} -ne 61 ]]; then
  echo "expected 61 shape assertions, got ${#ASSERTS[@]}" >&2
  exit 2
fi

echo "==> verifying ${#ASSERTS[@]} shape assertions"
for entry in "${ASSERTS[@]}"; do
  path=${entry%%|*}
  pattern=${entry#*|}
  if grep -qF -- "$pattern" "${TMP}/${path}"; then
    echo "OK ${path} :: ${pattern}"
  else
    echo "ASSERT FAIL ${path} :: ${pattern}" >&2
    failed=1
  fi
done

# G1-F4：负断言收窄到端点注册层——在锚点文件的 HttpApiEndpoint.<method>("name", "path")
# 注册表中确认无 question / abort 端点。语义=静态不存在性（源码锚点层级），
# 不是运行时不存在性：运行时负面由 evidence run 记录
# （R1 20261015T165420Z-4013391d：question 端点 404；R2 20261016T090000Z-0bb6ac6c：abort 404）。
NEG_ENDPOINT_PATTERNS='question|abort'
NEG_ENDPOINT_TARGETS=(
  packages/protocol/src/groups/session.ts
  packages/protocol/src/groups/permission.ts
  packages/protocol/src/groups/event.ts
  packages/protocol/src/groups/form.ts
)
for path in "${NEG_ENDPOINT_TARGETS[@]}"; do
  regs=$(grep -hoE 'HttpApiEndpoint\.[A-Za-z]+\("[^"]*", *"[^"]*"' "${TMP}/${path}" || true)
  if [[ -z "$regs" ]]; then
    echo "NEG ASSERT FAIL ${path} :: endpoint registration table empty (anchor shape drift?)" >&2
    failed=1
    continue
  fi
  total=$(printf '%s\n' "$regs" | wc -l | tr -d ' ')
  if printf '%s\n' "$regs" | grep -qiE "$NEG_ENDPOINT_PATTERNS"; then
    echo "NEG ASSERT FAIL ${path} :: question/abort present in endpoint registrations:" >&2
    printf '%s\n' "$regs" | grep -iE "$NEG_ENDPOINT_PATTERNS" >&2
    failed=1
  else
    echo "OK(absent) ${path} :: question/abort not in ${total} endpoint registrations (静态不存在性：源码锚点层级)"
  fi
done

echo
echo "==> live checklist（手工执行；事实清单见 internal/infrastructure/opencode2/CONTRACT.md，"
echo "    live probe 用 scripts/spike-v2/live-probe.py，参照 evidence run 20261015T165420Z-4013391d）:"
echo "    就绪判定 GET /api/session（anon=401 auth=200）；/doc 为 SPA；OpenAPI 在 /openapi.json"
echo "    事件 failureEvent=effect/http-api/stream/failure 为 effect 框架运行时字面量，"
echo "    非 opencode 源码可锚定符号——静态锚点只检测源码漂移，live/OpenAPI 语义由 evidence run 验证"
echo "    （R1 live/static-extract.openapi-forms.md A9）"

if [[ "$failed" -ne 0 ]]; then
  echo "==> FAILED: assertion failures above" >&2
  exit 1
fi
echo "==> OK: all v2 anchors verified at ${COMMIT:0:8} (v2.0.28)"
