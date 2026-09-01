#!/bin/zsh
# 打 v* tag 并推到 origin，由 CI 跑 goreleaser 正式发布。
#
# 用法:
#   ./scripts/release.sh [patch|minor|major] [-y]
#
# 默认 patch。不在本地跑 goreleaser。
set -euo pipefail

SCRIPT="$0"
REPO="$(cd "$(dirname "$SCRIPT")/.." && pwd)"
BUMP="patch"
ASSUME_YES=0

usage() {
  cat <<EOF
用法: $SCRIPT [patch|minor|major] [-y]

按当前最新 v* tag 自增版本并 push tag，触发 GitHub Actions 发布。

  patch|minor|major  可选，默认 patch
  -y                 跳过确认
  -h, --help         显示本说明

本脚本只做 git tag + git push；goreleaser 由 CI 执行。
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    patch|minor|major)
      BUMP="$1"
      shift
      ;;
    -y)
      ASSUME_YES=1
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

cd "$REPO"

latest="$(git tag -l 'v*' --sort=-v:refname | head -n 1 || true)"
if [[ -z "$latest" ]]; then
  base="0.0.0"
  echo "==> 未找到 v* tag，从 $base 起步"
else
  base="${latest#v}"
  echo "==> 最新 tag: $latest"
fi

if [[ ! "$base" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "error: 无法从 tag 解析 x.y.z 版本: ${latest:-<none>} (base=$base)" >&2
  exit 1
fi

IFS=. read -r major minor patch <<<"$base"
case "$BUMP" in
  major)
    major=$((major + 1))
    minor=0
    patch=0
    ;;
  minor)
    minor=$((minor + 1))
    patch=0
    ;;
  patch)
    patch=$((patch + 1))
    ;;
esac
new_tag="v${major}.${minor}.${patch}"

echo "==> 将创建并推送 tag: $new_tag ($BUMP)"
if [[ "$ASSUME_YES" != "1" ]]; then
  printf "确认继续？[y/N] "
  read -r ans
  if [[ ! "$ans" =~ ^[yY]$ ]]; then
    echo "已取消"
    exit 1
  fi
fi

echo "==> 前置检查"
if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: 工作区不干净，请先提交或 stash" >&2
  git status --porcelain >&2
  exit 1
fi

default_branch="$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)"
if [[ -z "$default_branch" ]]; then
  default_branch="main"
fi
current_branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$current_branch" != "$default_branch" ]]; then
  echo "error: 当前分支是 $current_branch，需要在默认分支 $default_branch 上打 tag" >&2
  exit 1
fi

git fetch origin "$default_branch" --tags
local_sha="$(git rev-parse HEAD)"
remote_sha="$(git rev-parse "origin/${default_branch}")"
if [[ "$local_sha" != "$remote_sha" ]]; then
  echo "error: 本地 $current_branch ($local_sha) 与 origin/${default_branch} ($remote_sha) 不同步" >&2
  exit 1
fi

if git rev-parse "$new_tag" >/dev/null 2>&1; then
  echo "error: tag 已存在: $new_tag" >&2
  exit 1
fi

echo "==> git tag $new_tag"
git tag "$new_tag"
echo "==> git push origin $new_tag"
git push origin "$new_tag"

echo "==> 已推送 $new_tag"
echo "    观察 CI: https://github.com/Endlex-net/ex-ocdeck/actions"
