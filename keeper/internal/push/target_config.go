package push

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrTargetNotConfigured means the SID is missing from
// `keeper.yml::push.targets[]`. The sentinel lets the caller (pushorch
// executeAsync) distinguish "the operator forgot to list the host" from
// transport errors (Authorize/Sign/Dial).
//
// Pilot form (S6, 2026-05-26): inline in keeper.yml. S7 will replace the
// resolver with a `souls.ssh_target jsonb` read; the sentinel stays the same.
var ErrTargetNotConfigured = errors.New("push: SID not configured in push.targets[]")

// Defaults for the pilot resolver's omitted `push.targets[].*` fields (see
// the [config.KeeperPushTarget] doc). Canonical unix conventions; the
// operator can override per-target with an explicit value.
const (
	defaultSSHPort  = 22
	defaultSSHUser  = "root"
	defaultSoulPath = "/usr/local/bin/soul"
)

// ConfigTargetResolver is the pilot resolver for SSH credentials from
// `keeper.yml::push.targets[]` (S6 SshDispatcher wire-up, [ADR-032
// amendment]). Indexes entries by SID in the constructor: O(1) lookup on the
// SendApply hot path.
//
// Resolve: SID → [SSHTarget] with defaults filled in (port 22 / user root /
// soul-path /usr/local/bin/soul, symmetric with docs/keeper/push.md).
// A SID without an entry → [ErrTargetNotConfigured] (fail-closed; the
// operator sees a clear message in push_runs.summary).
//
// S7 will replace this source with `souls.ssh_target jsonb` — the
// [TargetResolver] interface won't change, only the resolver's constructor
// in daemon wire-up.
type ConfigTargetResolver struct {
	byID map[string]SSHTarget
}

// NewConfigTargetResolver indexes the `push.targets[]` list by SID, filling
// in defaults for empty fields. Duplicate SIDs are already rejected by the
// schema phase (validatePush in shared/config/schema.go); as defense in
// depth, the last entry overwrites the previous one (though this shouldn't
// happen on a validated config).
func NewConfigTargetResolver(targets []config.KeeperPushTarget) *ConfigTargetResolver {
	byID := make(map[string]SSHTarget, len(targets))
	for _, t := range targets {
		if t.SID == "" {
			continue
		}
		byID[t.SID] = SSHTarget{
			Host:     t.SID,
			Port:     resolveInt(t.SSHPort, defaultSSHPort),
			User:     resolveStr(t.SSHUser, defaultSSHUser),
			SoulPath: resolveStr(t.SoulPath, defaultSoulPath),
		}
	}
	return &ConfigTargetResolver{byID: byID}
}

// Resolve implements [TargetResolver]. Lookup by SID; not found →
// ErrTargetNotConfigured.
func (r *ConfigTargetResolver) Resolve(_ context.Context, sid string) (SSHTarget, error) {
	t, ok := r.byID[sid]
	if !ok {
		return SSHTarget{}, fmt.Errorf("%w: %s", ErrTargetNotConfigured, sid)
	}
	return t, nil
}

func resolveInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func resolveStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
