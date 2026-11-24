#!/usr/bin/env bash
# Start an isolated LoopForge environment built from a branch of tencent/LoopForge.
# A <branch> of the form pull/<n> builds the head of pull request #n, forks included;
# tag:<name> builds a tag and commit:<full sha> a single commit.
#
# Usage:
#   ./lfenv.sh up <branch>      build the branch and start it (re-run to update)
#   ./lfenv.sh down <branch>    remove containers, data and the checkout
#   ./lfenv.sh logs <branch>    follow platform logs
#   ./lfenv.sh ls               list environments
#
# Environment:
#   LFENV_ROOT   state directory (default: ./.lfenv next to this script)
#   LFENV_REPO   git URL (default: https://github.com/tencent/LoopForge)
#   LFENV_PORT_BASE  first HTTP port to try (default: 18000)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="${LFENV_ROOT:-$here/.lfenv}"
repo="${LFENV_REPO:-https://github.com/tencent/LoopForge}"
mirror="$root/mirror.git"

fail() { printf '✗ %s\n' "$1" >&2; exit 1; }
step() { printf '\n▸ %s\n' "$1"; }

slug() {
  local s ref=$1
  [[ $ref =~ ^commit:([0-9a-f]{12}) ]] && ref="commit-${BASH_REMATCH[1]}"
  s=$(printf '%s' "$ref" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9' '-' | sed -E 's/-+/-/g; s/^-//; s/-$//' | cut -c1-40)
  [[ -n "$s" ]] || fail "invalid branch: $1"
  printf '%s' "$s"
}

port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

port_taken_by_env() {
  grep -qsx "$2=$1" "$root"/envs/*/src/.env
}

pick_port() {
  local var=$1 p=$2
  while ! port_free "$p" || port_taken_by_env "$p" "$var"; do p=$((p + 1)); done
  printf '%s' "$p"
}

sync_mirror() {
  mkdir -p "$root/envs"
  if [[ -d "$mirror" ]]; then
    git -C "$mirror" fetch --prune --quiet origin '+refs/heads/*:refs/heads/*'
  else
    git clone --bare --quiet "$repo" "$mirror"
    git -C "$mirror" config remote.origin.fetch '+refs/heads/*:refs/heads/*'
  fi
}

# resolve_commit prints the commit to build. Pull requests, tags and commits are
# fetched one at a time into refs outside the refs/heads/* that sync_mirror prunes.
resolve_commit() {
  local branch=$1
  if [[ $branch =~ ^pull/([0-9]+)$ ]]; then
    local ref="refs/pull/${BASH_REMATCH[1]}/head"
    git -C "$mirror" fetch --quiet origin "+$ref:$ref" || fail "pull request not found: #${BASH_REMATCH[1]}"
    git -C "$mirror" rev-parse --verify --quiet "$ref^{commit}" || fail "pull request not found: #${BASH_REMATCH[1]}"
  elif [[ $branch == tag:* ]]; then
    local ref="refs/tags/${branch#tag:}"
    git -C "$mirror" fetch --quiet origin "+$ref:$ref" || fail "tag not found: ${branch#tag:}"
    git -C "$mirror" rev-parse --verify --quiet "$ref^{commit}" || fail "tag not found: ${branch#tag:}"
  elif [[ $branch =~ ^commit:([0-9a-f]{40})$ ]]; then
    local sha=${BASH_REMATCH[1]}
    # GitHub serves any commit of the repository by hash, pull request commits included.
    git -C "$mirror" cat-file -e "$sha^{commit}" 2>/dev/null \
      || git -C "$mirror" fetch --quiet origin "+$sha:refs/lfenv/$sha" || fail "commit not found: $sha"
    git -C "$mirror" rev-parse --verify --quiet "$sha^{commit}" || fail "commit not found: $sha"
  else
    git -C "$mirror" rev-parse --verify --quiet "refs/heads/$branch^{commit}" || fail "branch not found: $branch"
  fi
}

compose_in() { (cd "$1" && docker compose --profile platform "${@:2}"); }

# Held for the rest of the process; fd 8 serializes git and port allocation, fd 9 one environment.
lock_env() {
  mkdir -p "$root/locks"
  exec 9>"$root/locks/$1.lock"
  flock -n 9 || fail "another operation on $1 is in progress"
}
lock_global() { mkdir -p "$root/locks"; exec 8>"$root/locks/global.lock"; flock 8; }
unlock_global() { flock -u 8; }

cmd_up() {
  local branch=$1 name dir src commit tag
  name=$(slug "$branch")
  dir="$root/envs/$name"
  src="$dir/src"
  lock_env "$name"
  local owner
  owner=$(sed -n 's/^LFENV_BRANCH=//p' "$src/.env" 2>/dev/null || true)
  [[ -z "$owner" || "$owner" == "$branch" ]] || fail "environment $name already belongs to branch $owner"

  step "Fetching $branch"
  lock_global
  sync_mirror
  commit=$(resolve_commit "$branch")

  if [[ -d "$src" ]]; then
    git -C "$src" checkout --quiet --detach "$commit"
  else
    mkdir -p "$dir"
    git -C "$mirror" worktree add --quiet --detach "$src" "$commit"
  fi
  tag="loopforge-platform:lfenv-$name"

  if [[ ! -f "$src/.env" ]]; then
    step 'Configuring'
    (cd "$src" && \
      COMPOSE_PROJECT_NAME="lfenv-$name" \
      AGENTFLOW_HTTP_PORT="$(pick_port AGENTFLOW_HTTP_PORT "${LFENV_PORT_BASE:-18000}")" \
      LOOPFORGE_MYSQL_PORT="$(pick_port LOOPFORGE_MYSQL_PORT 34000)" \
      LOOPFORGE_DATA_DIR="$dir/data" \
      bash scripts/configure.sh)
    {
      printf 'COMPOSE_FILE=compose.yaml:compose.dev.yaml:compose.lfenv.yaml\n'
      printf 'LOOPFORGE_DOCKER_SOCKET=/var/run/docker.sock\n'
      printf 'LFENV_BRANCH=%s\n' "$branch"
    } >>"$src/.env"
  fi
  unlock_global
  # Per-environment tag: compose.dev.yaml pins loopforge-platform:local for every checkout.
  cat >"$src/compose.lfenv.yaml" <<EOF
services:
  platform:
    image: $tag
    privileged: true
    environment:
      AGENTFLOW_ISOLATED_IMAGE: $tag
EOF
  mkdir -p "$dir/data/home" "$dir/data/workspaces"

  step "Building $branch @ ${commit:0:12}"
  LOOPFORGE_COMMIT="$commit" compose_in "$src" build platform
  step 'Starting'
  compose_in "$src" up -d --no-build --wait --wait-timeout 300
  step 'Installing bwrap'
  install_platform_bwrap "$src"
  (cd "$src" && bash scripts/install-schema-extensions.sh >/dev/null)
  printf '%s\n' "$commit" >"$dir/deployed"

  local port
  port=$(sed -n 's/^AGENTFLOW_HTTP_PORT=//p' "$src/.env")
  if [[ -n "${LFENV_POST_SCRIPT:-}" ]]; then
    run_post_script "$branch" "$name" "$commit" "$port" "$dir"
  fi
  touch "$dir/initialized"
  printf '\n✓ %s @ %s ready: http://%s:%s\n' "$branch" "${commit:0:12}" "$(hostname -I 2>/dev/null | awk '{print $1}')" "$port"
}

# Control-plane container only. Privileged is set in compose.lfenv.yaml; bwrap
# is installed into the running platform container and skipped when already present.
install_platform_bwrap() {
  compose_in "$1" exec -T platform sh -lc 'command -v bwrap >/dev/null 2>&1 || (apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bubblewrap)' \
    || fail "install bwrap in platform container failed"
}

# Runs after every successful start, rebuilds included, so it must be safe to
# repeat. LFENV_FIRST_RUN=1 until it has succeeded once for this environment.
# --foreground keeps it in this process group, so cancelling the job stops it.
run_post_script() {
  local branch=$1 name=$2 commit=$3 port=$4 dir=$5 first=1
  [[ -x "$LFENV_POST_SCRIPT" ]] || fail "post script is not executable: $LFENV_POST_SCRIPT"
  [[ -f "$dir/initialized" ]] && first=0
  step "Running post script $(basename "$LFENV_POST_SCRIPT")"
  (cd "$dir/src" && \
    LFENV_BRANCH="$branch" LFENV_SLUG="$name" LFENV_COMMIT="$commit" LFENV_FIRST_RUN="$first" \
    LFENV_PORT="$port" LFENV_BASE_URL="http://127.0.0.1:$port" LFENV_SRC="$dir/src" \
    LFENV_COMPOSE_PROJECT="lfenv-$name" AGENTFLOW_E2E_BASE="http://127.0.0.1:$port" \
    timeout --foreground "${LFENV_POST_TIMEOUT:-600}" "$LFENV_POST_SCRIPT") \
    || fail "post script failed (exit $?)"
}

cmd_down() {
  local name dir
  name=$(slug "$1")
  dir="$root/envs/$name"
  [[ -d "$dir" ]] || { printf '✓ nothing to remove for %s\n' "$1"; return 0; }
  lock_env "$name"
  [[ -f "$dir/src/.env" ]] && compose_in "$dir/src" down --volumes --remove-orphans || true
  docker image rm -f "loopforge-platform:lfenv-$name" >/dev/null 2>&1 || true
  # Sandbox containers run as root and leave root-owned files in the data directory.
  docker run --rm -v "$dir:/d" alpine:3.20 rm -rf /d/data >/dev/null 2>&1 || rm -rf "$dir/data"
  lock_global
  git -C "$mirror" worktree remove --force "$dir/src" 2>/dev/null || true
  [[ $1 =~ ^pull/([0-9]+)$ ]] && git -C "$mirror" update-ref -d "refs/pull/${BASH_REMATCH[1]}/head" 2>/dev/null || true
  [[ $1 =~ ^commit:([0-9a-f]{40})$ ]] && git -C "$mirror" update-ref -d "refs/lfenv/${BASH_REMATCH[1]}" 2>/dev/null || true
  rm -rf "$dir"
  git -C "$mirror" worktree prune
  printf '✓ removed %s\n' "$1"
}

cmd_logs() {
  local src="$root/envs/$(slug "$1")/src"
  [[ -f "$src/.env" ]] || fail "no environment for $1"
  compose_in "$src" logs -f platform
}

cmd_ls() {
  local env src branch port state
  printf '%-40s %-8s %-12s %s\n' BRANCH PORT COMMIT STATE
  for env in "$root"/envs/*/; do
    src="$env/src"
    [[ -f "$src/.env" ]] || continue
    branch=$(sed -n 's/^LFENV_BRANCH=//p' "$src/.env")
    port=$(sed -n 's/^AGENTFLOW_HTTP_PORT=//p' "$src/.env")
    state=$(compose_in "$src" ps --format '{{.Health}}' platform 2>/dev/null || true)
    printf '%-40s %-8s %-12s %s\n' "$branch" "$port" "$(git -C "$src" rev-parse --short=12 HEAD)" "${state:-stopped}"
  done
}

main() {
  case "${1:-}" in
    up) [[ $# -eq 2 ]] || fail 'usage: lfenv.sh up <branch>'; cmd_up "$2" ;;
    down) [[ $# -eq 2 ]] || fail 'usage: lfenv.sh down <branch>'; cmd_down "$2" ;;
    logs) [[ $# -eq 2 ]] || fail 'usage: lfenv.sh logs <branch>'; cmd_logs "$2" ;;
    ls) cmd_ls ;;
    *) sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
  esac
}
# One line, so bash has parsed the exit before running anything: editing this
# file while builds run must not make them read the new contents mid-way.
main "$@"; exit
