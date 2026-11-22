#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'Usage: scripts/install.sh [--bindir DIR] [--force] [--with-web] [chainsctl|chains|chains-go-service|all]'
}

bindir=
target=
force=false
with_web=false
while (($#)); do
  case "$1" in
    --bindir)
      if [[ $# -lt 2 || -z "$2" || "$2" == --* ]]; then usage >&2; exit 2; fi
      bindir=$2
      shift ;;
    --force) force=true ;;
    --with-web) with_web=true ;;
    -h|--help) usage; exit 0 ;;
    chainsctl|chains|chains-go-service|all)
      if [[ -n "$target" ]]; then usage >&2; exit 2; fi
      target=$1 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done

target=${target:-chainsctl}
if $with_web && [[ "$target" != chains && "$target" != all ]]; then
  printf '%s\n' '--with-web requires chains or all.' >&2
  exit 2
fi

if [[ "$target" == all ]]; then
  targets=(chainsctl chains chains-go-service)
else
  targets=("$target")
fi
mkdir -p -- "${bindir:-$HOME/.local/bin}"
bindir=$(cd -- "${bindir:-$HOME/.local/bin}" && pwd -P)
cd -- "$(dirname -- "$0")/.."

check_destinations() {
  local name dest installed_path
  for name in "${targets[@]}"; do
    dest="$bindir/$name"
    if [[ -d "$dest" ]]; then
      printf 'Destination is a directory: %s\n' "$dest" >&2
      return 1
    fi
    if ! $force && [[ -e "$dest" || -L "$dest" ]]; then
      installed_path=
      if [[ -f "$dest" && ! -L "$dest" ]]; then
        installed_path=$(go version -m "$dest" 2>/dev/null | awk '$1 == "path" {print $2}') || true
      fi
      if [[ "$installed_path" != "chains-automation/cmd/$name" ]]; then
        printf 'Existing file at %s. Use --force to replace it.\n' "$dest" >&2
        return 1
      fi
    fi
  done
}

check_destinations
stage=$(mktemp -d "$bindir/.chains-install.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if $with_web; then
  npm ci --prefix web
  npm run build --prefix web
fi

for name in "${targets[@]}"; do
  cgo=1
  flags=()
  if [[ "$name" == chainsctl ]]; then cgo=0; fi
  if [[ "$name" == chains ]] && $with_web; then flags=(-tags webui); fi
  GOBIN="$stage" CGO_ENABLED="$cgo" go install -trimpath -buildvcs=false "${flags[@]}" "./cmd/$name"
done

check_destinations
for name in "${targets[@]}"; do
  mv -fT -- "$stage/$name" "$bindir/$name"
  printf 'Installed %s\n' "$bindir/$name"
done
