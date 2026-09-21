package config

import (
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// devStandConfig is the committed render of dev/keeper.dev.yml.tmpl for the
// default stand — what `make dev-keeper` actually starts the daemon with.
// `make check-stand-template` keeps the two byte-identical, so guarding this
// file guards the template.
const devStandConfig = "../../dev/keeper.dev.yml"

// The dev stand has to come up on a clean machine with nothing but the repo and
// `make dev-provision`. Anything in its config that needs material minted out of
// band turns "bring up a stand" into "first repair the stand", and every session
// that owes a live-stand acceptance pays for it.
//
// `push.transport: teleport` was exactly that (NIM-266): it requires
// `teleport.identity_file`, which comes from `tctl auth sign` against a live
// Teleport the dev stand does not have, and keeper refuses to start without it —
// `build bootstrap teleport dialer: identity file could not be decoded`. Nothing
// in the repo creates that file, and the template said so three lines above the
// block that selected it ("the dev stand has no Teleport").
//
// Teleport stays available as an opt-in stand profile (DEV_PUSH_TRANSPORT, see
// dev/stand-env.sh); what may not come back is a committed default that cannot
// start.
func TestDevStand_PushTransportNeedsNoOutOfBandIdentity(t *testing.T) {
	cfg, _, _, err := LoadKeeper(filepath.Clean(devStandConfig), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeper(%s): %v", devStandConfig, err)
	}
	if cfg.Push == nil {
		return // no push block — nothing to deliver, nothing to mint
	}
	if cfg.Push.Transport == PushTransportTeleport {
		identity := "<unset>"
		if cfg.Push.Teleport != nil {
			identity = cfg.Push.Teleport.IdentityFile
		}
		t.Errorf("%s selects push.transport: teleport, so `make dev-keeper` fails to start on a clean machine: "+
			"the identity file it needs (%s) is minted by `tctl auth sign` against a live Teleport and nothing in "+
			"the repo creates it. Keep the committed default at %q and opt into teleport with DEV_PUSH_TRANSPORT=teleport.",
			devStandConfig, identity, PushTransportDirect)
	}
}

// The stand config is also the worked example an operator copies from, so it has
// to be valid on its own terms — a schema or semantic error here is a defect
// whether or not the daemon happens to tolerate it.
func TestDevStand_ConfigValidates(t *testing.T) {
	_, _, diags, err := LoadKeeper(filepath.Clean(devStandConfig), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeper(%s): %v", devStandConfig, err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Errorf("%s: %s [%s] %s", devStandConfig, d.Phase, d.Code, d.Message)
		}
	}
}
