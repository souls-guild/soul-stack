package rbac

import (
	"sort"
	"strings"
	"testing"
)

// The catalog is closed and `<resource>.*` expands to every known action of that
// resource ([TestEnforcer_ConsoleCoveredByResourceWildcard]), so adding an action
// widens every wildcard grant an operator already issued — at upgrade time, with
// no role edit and nothing in the audit trail to notice. That is a property, not
// a defect, and the only place it can be handled is the release notes.
//
// These rosters are therefore enumerated verbatim for upgraders in CHANGELOG.md
// (§ Upgrade notes) and in docs/keeper/rbac.md. A prose list rots silently: the
// NIM-241 ticket text itself enumerated `soul.*` from memory and omitted
// `soul.traits-assign`. Adding an action to one of these resources fails here so
// the enumeration is revisited in the same change.
//
// Scoped to the four resources whose wildcard expansion is a privilege question
// rather than bookkeeping — a root shell, the right to mint untracked privilege,
// the delegation map, and an operation that moves a host between RBAC scopes.
// Other resources are deliberately not pinned; this is a release-notes guard, not
// a catalog snapshot.
func TestCatalog_WildcardRostersPinnedForReleaseNotes(t *testing.T) {
	want := map[string][]string{
		// soul.console is the R5 addition: an interactive PTY as the Soul
		// daemon's user (typically root), the MCP one-shot command, and
		// playback of anyone's recorded session.
		"soul": {
			"soul.console",
			"soul.coven-assign",
			"soul.create",
			"soul.issue-token",
			"soul.list",
			"soul.ssh-target-update",
			"soul.traits-assign",
		},
		// role.create-root and role.list-all are the R5 additions. Both are
		// checked bare, so only an UNRESTRICTED role.* covers them.
		"role": {
			"role.create",
			"role.create-root",
			"role.delete",
			"role.grant-operator",
			"role.list",
			"role.list-all",
			"role.revoke-operator",
			"role.update",
		},
		// synod.list-all is the R5 addition and the mirror of role.list-all:
		// the whole group catalog — every bundle and every roster, i.e. which
		// packages of privilege exist and who holds them. Checked bare, so
		// only an UNRESTRICTED synod.* covers it.
		"synod": {
			"synod.add-operator",
			"synod.create",
			"synod.delete",
			"synod.grant-role",
			"synod.list",
			"synod.list-all",
			"synod.remove-operator",
			"synod.revoke-role",
			"synod.update",
		},
		// incarnation.bind-member / unbind-member are the R5 additions: under
		// ADR-080 a bind hands the host its incarnation's labels, so it moves
		// the host into the scope of every role scoped to them.
		"incarnation": {
			"incarnation.bind-member",
			"incarnation.check-drift",
			"incarnation.create",
			"incarnation.destroy",
			"incarnation.get",
			"incarnation.history",
			"incarnation.list",
			"incarnation.rerun-last",
			"incarnation.run",
			"incarnation.traits-set",
			"incarnation.unbind-member",
			"incarnation.unlock",
			"incarnation.upgrade",
			"incarnation.view-secrets",
		},
	}

	for resource, wantActions := range want {
		got := actionsOfResource(resource)
		if strings.Join(got, "\n") == strings.Join(wantActions, "\n") {
			continue
		}
		t.Errorf("`%s.*` no longer expands to the roster the release notes enumerate.\n"+
			" got: %v\nwant: %v\n"+
			"Every holder of `%s.*` gains an added action on upgrade. Update the roster here, "+
			"the § Upgrade notes list in CHANGELOG.md and docs/keeper/rbac.md in the same change.",
			resource, got, wantActions, resource)
	}
}

// actionsOfResource returns the sorted `<resource>.<action>` names a
// `<resource>.*` grant expands to.
func actionsOfResource(resource string) []string {
	prefix := resource + "."
	names := make([]string, 0, 8)
	for name := range AllowedPermissions {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
