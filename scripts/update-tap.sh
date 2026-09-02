#!/bin/zsh
# 根据 goreleaser 产物渲染 Homebrew Formula 并推到 Endlex-net/homebrew-tap。
#
# 用法:
#   scripts/update-tap.sh --tag vX.Y.Z [--tap-dir <path>] [--dry-run]
#
# 从仓库根目录的 dist/checksums.txt 读取 darwin 产物 sha256，
# 渲染 Formula/ocdeck.rb。本脚本不 clone tap；CI 由 workflow 显式 clone。
set -euo pipefail

SCRIPT="$0"
REPO="$(cd "$(dirname "$SCRIPT")/.." && pwd)"
CHECKSUMS="$REPO/dist/checksums.txt"
TAG=""
TAP_DIR=""
DRY_RUN=0
ASSET_ARM="ocdeck_darwin_arm64.tar.gz"
ASSET_AMD="ocdeck_darwin_amd64.tar.gz"

usage() {
  cat <<EOF
用法: $SCRIPT --tag vX.Y.Z [--tap-dir <path>] [--dry-run]

根据 dist/checksums.txt 渲染 Homebrew Formula（macOS 双架构）。

  --tag vX.Y.Z        必填，GitHub Release tag（保留 v 前缀）
  --tap-dir <path>    非 dry-run 必填，homebrew-tap 的 git clone 路径
  --dry-run           只把 formula 打到 stdout 并 ruby -c，不写不推
  -h, --help          显示本说明

非 dry-run 会写入 <tap-dir>/Formula/ocdeck.rb 后 commit + push。
EOF
}

sha_for() {
  local file="$1" hash
  hash="$(awk -v f="$file" '$2 == f { print $1; found=1 } END { exit !found }' "$CHECKSUMS")" || {
    echo "error: $CHECKSUMS 中未找到 $file" >&2
    exit 1
  }
  print -r -- "$hash"
}

render_formula() {
  local version="$1" tag="$2" sha_arm="$3" sha_amd="$4"
  local url_base="https://github.com/Endlex-net/ex-ocdeck/releases/download/${tag}"
  cat <<EOF
class Ocdeck < Formula
  desc "自托管的 opencode 任务编排 Web 控制台"
  homepage "https://github.com/Endlex-net/ex-ocdeck"
  version "${version}"

  on_arm do
    url "${url_base}/${ASSET_ARM}"
    sha256 "${sha_arm}"
  end

  on_intel do
    url "${url_base}/${ASSET_AMD}"
    sha256 "${sha_amd}"
  end

  depends_on "tmux"
  depends_on "git"

  def install
    bin.install "ocdeck-server"
  end

  service do
    run [opt_bin/"ocdeck-server"]
    run_at_load true
    keep_alive true
    # launchd 默认 PATH 不含 Homebrew 前缀，opencode/tmux 会找不到
    environment_variables PATH: std_service_path_env
    log_path var/"log/ocdeck.log"
    error_log_path var/"log/ocdeck.log"
  end

  def caveats
    <<~EOS
      首次启动前必须创建 ~/.config/ocdeck/env 且至少包含：
        OCDECK_TOKEN=<token>
      可选：OCDECK_LISTEN_PORT / OCDECK_SERVE_PORT_RANGE 等。
      改配置后执行：brew services restart ocdeck
    EOS
  end
end
EOF
}

ruby_check() {
  local file="$1"
  if ! command -v ruby >/dev/null 2>&1; then
    echo "==> 未找到 ruby，跳过 ruby -c"
    return 0
  fi
  echo "==> ruby -c 校验语法"
  ruby -c "$file"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      TAG="${2:-}"
      if [[ -z "$TAG" ]]; then
        echo "error: --tag 需要 vX.Y.Z" >&2
        exit 2
      fi
      shift 2
      ;;
    --tap-dir)
      TAP_DIR="${2:-}"
      if [[ -z "$TAP_DIR" ]]; then
        echo "error: --tap-dir 需要路径" >&2
        exit 2
      fi
      shift 2
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: 未知参数: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$TAG" ]]; then
  echo "error: 缺少 --tag" >&2
  usage >&2
  exit 2
fi
if [[ ! "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: --tag 需为 vX.Y.Z，当前: $TAG" >&2
  exit 2
fi
VERSION="${TAG#v}"

if [[ "$DRY_RUN" != "1" && -z "$TAP_DIR" ]]; then
  echo "error: 非 --dry-run 需要 --tap-dir" >&2
  usage >&2
  exit 2
fi

if [[ ! -f "$CHECKSUMS" ]]; then
  echo "error: 未找到 $CHECKSUMS" >&2
  echo "  先在仓库根目录跑 goreleaser，生成 dist/checksums.txt" >&2
  exit 1
fi

echo "==> tag: $TAG (version=$VERSION)"
echo "==> 读取 $CHECKSUMS"
SHA_ARM="$(sha_for "$ASSET_ARM")"
SHA_AMD="$(sha_for "$ASSET_AMD")"
echo "    $ASSET_ARM $SHA_ARM"
echo "    $ASSET_AMD $SHA_AMD"

FORMULA="$(render_formula "$VERSION" "$TAG" "$SHA_ARM" "$SHA_AMD")"

if [[ "$DRY_RUN" == "1" ]]; then
  echo "==> --dry-run：渲染 formula 到 stdout"
  print -r -- "$FORMULA"
  DRY_TMP="$(mktemp)"
  print -r -- "$FORMULA" > "$DRY_TMP"
  ruby_check "$DRY_TMP"
  rm -f "$DRY_TMP"
  echo "==> dry-run 完成（未写入 tap、未 push）"
  exit 0
fi

TAP_DIR="$(cd "$TAP_DIR" && pwd)"
if [[ ! -d "$TAP_DIR/.git" ]]; then
  echo "error: --tap-dir 不是 git clone: $TAP_DIR" >&2
  exit 1
fi

echo "==> tap: $TAP_DIR"
cd "$TAP_DIR"
if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: tap 工作区不干净，请先处理后再跑" >&2
  git status >&2
  exit 1
fi

FORMULA_DST="$TAP_DIR/Formula/ocdeck.rb"
echo "==> 写入 $FORMULA_DST"
mkdir -p "$(dirname "$FORMULA_DST")"
print -r -- "$FORMULA" > "$FORMULA_DST"
ruby_check "$FORMULA_DST"

echo "==> git add/commit/push"
git add Formula/ocdeck.rb
git commit -m "ocdeck ${VERSION}"
git push

echo "==> 已推送 ocdeck ${VERSION} 到 $(git remote get-url origin)"
