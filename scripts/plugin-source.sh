#!/usr/bin/env bash
# plugin-source.sh - where this tree gets the sources of a plugin that no longer
# lives in it (NIM-868).
#
# `examples/module/redis` and `examples/module/vmlocal` were the last two Go modules
# under `examples/`, which ADR-011 reserves for non-Go artifacts. They left for one
# repository each, and that took the sources out from under three readers that had
# been building them in place: `check-plugin-schema`, the L3b live fixture
# (harness.BuildRedisPlugin) and `dev/provision.sh`.
#
# The mechanism is the one NIM-876 chose for the out-of-tree SERVICE repositories, and
# the argument transfers whole, so it is not restated here - see
# tests/e2e-live/harness/servicecatalog.go. In short: a PINNED COMMIT in a cache
# outside the repository. Cloning per run would make a blocking gate depend on
# github.com being up; reading a checkout on disk would make it prove whatever happened
# to be in that directory.
#
# ★ WHY SHELL, when servicecatalog.go is Go. That catalog has exactly one kind of
# reader and it is Go. This one has two - a Makefile recipe and the Go harness - and the
# pin must have ONE home or the two drift, which is the NIM-507 shape (a list spelled
# twice, an entry present in one copy and not the other). Shell is what both can call
# without one of them growing a parser.
#
# ★ WHAT THIS DOES NOT DO: bump a pin. A pin names what a gate will prove, so moving it
# is a decision - the new commit has to have been run. `make check` against the
# candidate commit is the whole procedure.
#
# Usage:
#   scripts/plugin-source.sh dir <alias>    # print the tree at the pin, filling the cache
#   scripts/plugin-source.sh prime          # fill the cache for every pinned plugin
#   scripts/plugin-source.sh aliases        # every alias in the catalog
#   scripts/plugin-source.sh overrides      # aliases currently pointed at a working tree
#
# Environment:
#   SOUL_STACK_PLUGIN_CACHE          cache root (default: $XDG_CACHE_HOME/soul-stack/plugins)
#   SOUL_STACK_PLUGIN_OFFLINE=1      forbid fetching; a cache miss then fails by name
#   SOUL_STACK_PLUGIN_REMOTE_<A>     fetch the pinned commit from elsewhere (mirror, local
#                                    clone). The pin still holds, so the verdict is unchanged.
#   SOUL_STACK_PLUGIN_DIR_<A>        run a working tree INSTEAD of the pin, for developing a
#                                    plugin and the engine together. This weakens every
#                                    verdict resting on it, so `make e2e-live-gate` refuses
#                                    to start while it is set - see `overrides`.
set -euo pipefail

# The catalog: one row per plugin, `alias|url|commit|why`. The commit is full 40-hex -
# a tag is movable, and "the gate builds v1.2.0" stops being a fact the moment someone
# retags.
#
# `url` is https rather than ssh: the cache is filled unattended, on machines with no
# key for these repositories.
plugin_catalog() {
  cat <<'CATALOG'
redis|https://github.com/soul-stack-plugin/redis.git|1c210084c852c311a0249b654568e7e987524a26|the artifact examples/module/redis/schema.json is vendored from, and the SoulModule the L3b live tier delivers
vmlocal|https://github.com/soul-stack-plugin/vmlocal.git|71eb17bc3d8864c8e6f0cd6bde0a05a7dcfd1066|the libvirt machine provider the keeper-side ssh lane provisions a real VM through
CATALOG
}

die() { echo "plugin-source.sh: $*" >&2; exit 1; }

# env_suffix - the alias as it appears in an environment variable name: upper case, and
# every character git allows in a repository name but sh does not allow in an identifier
# replaced by `_`.
env_suffix() {
  printf '%s' "$1" | tr '[:lower:]' '[:upper:]' | tr -c 'A-Z0-9' '_' | sed 's/_*$//'
}

catalog_row() {
  local alias="$1" row
  row="$(plugin_catalog | grep "^${alias}|" || true)"
  if [ -z "${row}" ]; then
    die "no plugin ${alias} in the catalog; it carries: $(plugin_catalog | cut -d'|' -f1 | tr '\n' ' ')"
  fi
  printf '%s' "${row}"
}

cache_root() {
  if [ -n "${SOUL_STACK_PLUGIN_CACHE:-}" ]; then
    printf '%s' "${SOUL_STACK_PLUGIN_CACHE}"
    return
  fi
  local base="${XDG_CACHE_HOME:-${HOME}/.cache}"
  printf '%s/soul-stack/plugins' "${base}"
}

offline() {
  case "$(printf '%s' "${SOUL_STACK_PLUGIN_OFFLINE:-}" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes) return 0;;
    *) return 1;;
  esac
}

# ensure_tree - the path holding <alias> at its pinned commit, fetching and extracting it
# if the cache does not have it yet. Idempotent.
ensure_tree() {
  local alias="$1" row url commit suffix tree repo staging
  row="$(catalog_row "${alias}")"
  url="$(printf '%s' "${row}" | cut -d'|' -f2)"
  commit="$(printf '%s' "${row}" | cut -d'|' -f3)"
  suffix="$(env_suffix "${alias}")"

  case "${commit}" in
    [0-9a-f]*) [ "${#commit}" -eq 40 ] || die "${alias}: pinned commit ${commit} is not a full 40-hex object name";;
    *) die "${alias}: pinned commit ${commit} is not a full 40-hex object name";;
  esac

  # A named working tree wins over the pin, loudly enough that `overrides` can report it.
  local override
  eval "override=\${SOUL_STACK_PLUGIN_DIR_${suffix}:-}"
  if [ -n "${override}" ]; then
    [ -d "${override}" ] || die "${alias}: SOUL_STACK_PLUGIN_DIR_${suffix}=${override} is not a directory"
    printf '%s' "${override}"
    return
  fi

  local root; root="$(cache_root)"
  tree="${root}/tree/${alias}/${commit}"
  # The commit is IN THE PATH, so an extracted tree cannot hold anything but the commit it
  # is named after, and a pin bump adds a directory instead of rewriting one.
  if [ -d "${tree}" ] && [ -n "$(ls -A "${tree}" 2>/dev/null)" ]; then
    printf '%s' "${tree}"
    return
  fi

  eval "local remote=\${SOUL_STACK_PLUGIN_REMOTE_${suffix}:-}"
  [ -n "${remote}" ] && url="${remote}"

  repo="${root}/repo/${alias}.git"
  mkdir -p "$(dirname "${repo}")"
  if [ ! -f "${repo}/HEAD" ]; then
    git init --bare -q "${repo}" || die "${alias}: git init ${repo} failed"
  fi

  if ! git --git-dir="${repo}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    if offline; then
      die "${alias}: commit ${commit} is not in the cache and SOUL_STACK_PLUGIN_OFFLINE forbids fetching it.
  The cache is ${repo}.
  Prime it on a machine with a route to ${url}:
      make plugin-sources"
    fi
    GIT_TERMINAL_PROMPT=0 GIT_ASKPASS= SSH_ASKPASS= \
      git --git-dir="${repo}" fetch --quiet --tags "${url}" '+refs/heads/*:refs/remotes/origin/*' \
      || die "${alias}: git fetch ${url} failed"
  fi
  if ! git --git-dir="${repo}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    die "${alias}: ${url} does not carry commit ${commit}.
  The pin names a commit that is not published there - the usual cause is a pin bumped to a
  local commit that was never pushed. What these gates prove has to be fetchable by whoever
  runs them next."
  fi

  # Extract into a sibling and rename, so a run interrupted mid-extraction cannot leave a
  # half-populated directory that the non-empty check above would read as a warm cache.
  staging="${tree}.partial"
  rm -rf "${staging}"
  mkdir -p "${staging}" "$(dirname "${tree}")"
  git --git-dir="${repo}" archive --format=tar "${commit}" | tar -x -C "${staging}" \
    || die "${alias}: extracting ${commit} failed"
  mv "${staging}" "${tree}"
  printf '%s' "${tree}"
}

cmd="${1:-}"
case "${cmd}" in
  dir)
    [ $# -eq 2 ] || die "usage: plugin-source.sh dir <alias>"
    ensure_tree "$2"
    ;;
  prime)
    root="$(cache_root)"
    echo "plugin-source.sh: priming the plugin source cache in ${root}"
    while IFS='|' read -r alias _url _commit why; do
      [ -n "${alias}" ] || continue
      tree="$(ensure_tree "${alias}")"
      echo "  ${alias}: ${tree}"
      echo "      ${why}"
    done < <(plugin_catalog)
    ;;
  aliases)
    plugin_catalog | cut -d'|' -f1
    ;;
  overrides)
    while IFS='|' read -r alias _rest; do
      [ -n "${alias}" ] || continue
      suffix="$(env_suffix "${alias}")"
      eval "override=\${SOUL_STACK_PLUGIN_DIR_${suffix}:-}"
      if [ -n "${override}" ]; then echo "${alias}=${override}"; fi
    done < <(plugin_catalog)
    # Nothing overridden is the normal case, and it is an ANSWER, not a failure: callers
    # decide on the emptiness of the output, so this must not exit non-zero under `set -e`.
    ;;
  *)
    die "usage: plugin-source.sh {dir <alias>|prime|aliases|overrides}"
    ;;
esac
