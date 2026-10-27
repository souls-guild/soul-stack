#!/usr/bin/env bash
# destiny-source.sh - where this tree gets a destiny it reads but does not contain (NIM-1062).
#
# Readers: `make trial` (the L0 corpus of `examples/service/dragonfly`), the render, config
# and lint tests that read the destiny's files, the L3b live fixture and `dev/provision.sh`.
# Do not vendor one of these destinies back under examples/destiny: a second copy drifts from
# the published one - the last one ran a binary-installed redis-server without the systemd
# sandbox the public destiny has - and `make trial` and TestExamples_PinnedDestinies refuse it.
#
# The mechanism is scripts/plugin-source.sh's, for the reason given there and in
# tests/e2e-live/harness/servicecatalog.go: a PINNED COMMIT in a cache outside the
# repository, so a gate neither depends on github.com being up nor proves whatever happens
# to be in some directory.
#
# ★ ONE COLUMN MORE THAN THE PLUGIN CATALOG: the tag. A service names a destiny by git ref
# (ADR-007, `{ name: redis, ref: v1.0.0 }`), so the readers here stand in for "redis at
# v1.0.0" and the pin has to BE that tag. A fetch checks that the tag resolves to the pinned
# commit and refuses otherwise; keeper/internal/trial's TestExamples_PinnedDestinies holds
# every corpus service's ref to the tag column.
#
# ★ WHAT THIS DOES NOT DO: bump a pin. A pin names what the gate proves, so moving it is a
# decision - `make check` against the candidate commit is the whole procedure.
#
# Usage:
#   scripts/destiny-source.sh dir <alias>    # print the tree at the pin, filling the cache
#   scripts/destiny-source.sh tag <alias>    # the tag the pin stands for
#   scripts/destiny-source.sh prime          # fill the cache for every catalog destiny
#   scripts/destiny-source.sh aliases        # every alias in the catalog
#   scripts/destiny-source.sh overrides      # aliases currently pointed at a working tree
#
# Environment:
#   SOUL_STACK_DESTINY_CACHE          cache root (default: $XDG_CACHE_HOME/soul-stack/destinies)
#   SOUL_STACK_DESTINY_OFFLINE=1      forbid fetching; a cache miss then fails by name
#   SOUL_STACK_DESTINY_REMOTE_<A>     fetch the pinned commit from elsewhere (mirror, local
#                                     clone). The pin still holds, so the verdict is unchanged.
#   SOUL_STACK_DESTINY_DIR_<A>        run a working tree INSTEAD of the pin, for developing a
#                                     destiny and the engine together. This weakens every
#                                     verdict resting on it, so `make e2e-live-gate` refuses
#                                     to start while it is set - see `overrides`.

set -euo pipefail

# The catalog: one row per destiny, `alias|url|commit|tag|why`. The alias is the destiny's
# name, because readers resolve it as `{name}`; the commit is full 40-hex, because a tag
# is movable.
destiny_catalog() {
  cat <<'CATALOG'
redis|https://github.com/soul-stack-destiny/redis.git|d10e2bb0cf9d494cb779e4b41c9a17e21d89be4d|v1.0.0|the sentinel plane of examples/service/dragonfly, the redis templates the render tests read, and the brick the L3b redis service applies
CATALOG
}

die() { echo "destiny-source.sh: $*" >&2; exit 1; }

env_suffix() {
  printf '%s' "$1" | tr '[:lower:]' '[:upper:]' | tr -c 'A-Z0-9' '_' | sed 's/_*$//'
}

catalog_row() {
  local alias="$1" row
  row="$(destiny_catalog | grep "^${alias}|" || true)"
  if [ -z "${row}" ]; then
    die "no destiny ${alias} in the catalog; it carries: $(destiny_catalog | cut -d'|' -f1 | tr '\n' ' ')"
  fi
  printf '%s' "${row}"
}

cache_root() {
  if [ -n "${SOUL_STACK_DESTINY_CACHE:-}" ]; then
    printf '%s' "${SOUL_STACK_DESTINY_CACHE}"
    return
  fi
  printf '%s/soul-stack/destinies' "${XDG_CACHE_HOME:-${HOME}/.cache}"
}

offline() {
  case "$(printf '%s' "${SOUL_STACK_DESTINY_OFFLINE:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes) return 0;;
    *) return 1;;
  esac
}

# ensure_tree - the path holding <alias> at its pinned commit, fetching and extracting it
# if the cache does not have it yet. Idempotent.
ensure_tree() {
  local alias="$1" row url commit tag suffix tree repo staging
  row="$(catalog_row "${alias}")"
  url="$(printf '%s' "${row}" | cut -d'|' -f2)"
  commit="$(printf '%s' "${row}" | cut -d'|' -f3)"
  tag="$(printf '%s' "${row}" | cut -d'|' -f4)"
  suffix="$(env_suffix "${alias}")"

  case "${commit}" in
    *[!0-9a-f]*|'') die "${alias}: pinned commit ${commit} is not a full 40-hex object name";;
  esac
  [ "${#commit}" -eq 40 ] || die "${alias}: pinned commit ${commit} is not a full 40-hex object name"

  local override
  eval "override=\${SOUL_STACK_DESTINY_DIR_${suffix}:-}"
  if [ -n "${override}" ]; then
    [ -d "${override}" ] || die "${alias}: SOUL_STACK_DESTINY_DIR_${suffix}=${override} is not a directory"
    printf '%s' "${override}"
    return
  fi

  local root; root="$(cache_root)"
  tree="${root}/tree/${alias}/${commit}"
  if [ -d "${tree}" ] && [ -n "$(ls -A "${tree}" 2>/dev/null)" ]; then
    printf '%s' "${tree}"
    return
  fi

  eval "local remote=\${SOUL_STACK_DESTINY_REMOTE_${suffix}:-}"
  [ -n "${remote}" ] && url="${remote}"

  repo="${root}/repo/${alias}.git"
  mkdir -p "$(dirname "${repo}")"
  # Two gates fill a cold cache at once (GATE_SLOTS=2): without the lock their fetches race
  # on the bare repo's ref locks. The lock is released when this process exits. Where flock
  # is missing (macOS) concurrent cold fills are not serialized: a loser may fail loudly on
  # `git init` or `git fetch`, and one that gets as far as installing the tree leaves the
  # winner's in place (below).
  if command -v flock >/dev/null 2>&1; then
    exec 9>"${repo}.lock"
    flock 9
    if [ -d "${tree}" ] && [ -n "$(ls -A "${tree}" 2>/dev/null)" ]; then
      printf '%s' "${tree}"
      return
    fi
  fi
  if [ ! -f "${repo}/HEAD" ]; then
    git init --bare -q "${repo}" || die "${alias}: git init ${repo} failed"
  fi

  # Tags are force-updated: a tag moved upstream must reach the check below and be refused
  # there, not be kept at its old value by the default no-clobber rule.
  fetch_pin() {
    if offline; then
      die "${alias}: ${1} and SOUL_STACK_DESTINY_OFFLINE forbids fetching.
  The cache is ${repo}.
  Prime it on a machine with a route to ${url}:
      make destiny-sources"
    fi
    GIT_TERMINAL_PROMPT=0 GIT_ASKPASS= SSH_ASKPASS= \
      git --git-dir="${repo}" fetch --quiet "${url}" \
        '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*' \
      || die "${alias}: git fetch ${url} failed"
  }
  tag_commit() {
    git --git-dir="${repo}" rev-parse --verify --quiet "refs/tags/${tag}^{commit}" || true
  }

  local fetched=""
  if ! git --git-dir="${repo}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    fetch_pin "commit ${commit} is not in the cache"
    fetched=1
  fi
  if ! git --git-dir="${repo}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    die "${alias}: ${url} does not carry commit ${commit}.
  The pin names a commit that is not published there - the usual cause is a pin bumped to a
  local commit that was never pushed. What these gates prove has to be fetchable by whoever
  runs them next."
  fi
  local tagged
  tagged="$(tag_commit)"
  # A cached commit with a missing or different tag may only mean the tag was published after
  # the last fetch, so the refusal below is made against a fresh copy of the tags.
  if [ "${tagged}" != "${commit}" ] && [ -z "${fetched}" ]; then
    fetch_pin "tag ${tag} does not name ${commit} in the cache"
    tagged="$(tag_commit)"
  fi
  if [ "${tagged}" != "${commit}" ]; then
    die "${alias}: tag ${tag} names ${tagged:-nothing} in ${url}, not the pinned ${commit}.
  The services that declare ${alias} at ${tag} would get a different tree from the one these
  gates prove. Either the tag moved upstream or the pin is wrong - settle which before
  changing either."
  fi

  # Extract into a sibling and rename, so a run interrupted mid-extraction cannot leave a
  # half-populated directory that the non-empty check above would read as a warm cache.
  # `rmdir` clears an empty directory at the tree's path, which `mv` would move into. There
  # is no check before the rename: when another writer installs the tree first, `mv` lands
  # this copy INSIDE that tree instead, and it is removed again - by the line after `mv`, or
  # by the trap on a signal in between. The name is the mktemp one, which no destiny file
  # can carry.
  mkdir -p "$(dirname "${tree}")"
  # Explicit, not left to `set -e`: `prime` runs this inside `$(...)`, where errexit is off,
  # and an empty staging would turn the trap below into `rm -rf "${tree}/"`.
  staging="$(mktemp -d "${tree}.partial.XXXXXXXX")" && [ -n "${staging}" ] \
    || die "${alias}: creating a staging directory beside ${tree} failed"
  trap "rm -rf $(printf '%q' "${staging}") $(printf '%q' "${tree}/${staging##*/}")" EXIT
  git --git-dir="${repo}" archive --format=tar "${commit}" | tar -x -C "${staging}" \
    || die "${alias}: extracting ${commit} failed"
  rmdir "${tree}" 2>/dev/null || true
  mv "${staging}" "${tree}" || die "${alias}: installing ${tree} failed"
  rm -rf "${tree:?}/${staging##*/}"
  printf '%s' "${tree}"
}

cmd="${1:-}"
case "${cmd}" in
  dir)
    [ $# -eq 2 ] || die "usage: destiny-source.sh dir <alias>"
    ensure_tree "$2"
    ;;
  tag)
    [ $# -eq 2 ] || die "usage: destiny-source.sh tag <alias>"
    catalog_row "$2" | cut -d'|' -f4
    ;;
  prime)
    root="$(cache_root)"
    echo "destiny-source.sh: priming the destiny source cache in ${root}"
    while IFS='|' read -r alias _url _commit tag why; do
      [ -n "${alias}" ] || continue
      tree="$(ensure_tree "${alias}")"
      echo "  ${alias}@${tag}: ${tree}"
      echo "      ${why}"
    done < <(destiny_catalog)
    ;;
  aliases)
    destiny_catalog | cut -d'|' -f1
    ;;
  overrides)
    while IFS='|' read -r alias _rest; do
      [ -n "${alias}" ] || continue
      suffix="$(env_suffix "${alias}")"
      eval "override=\${SOUL_STACK_DESTINY_DIR_${suffix}:-}"
      if [ -n "${override}" ]; then echo "${alias}=${override}"; fi
    done < <(destiny_catalog)
    ;;
  *)
    die "usage: destiny-source.sh {dir <alias>|tag <alias>|prime|aliases|overrides}"
    ;;
esac
