package bootstrap

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PendingKeyFile is the private key of an onboarding that has started but not
// finished, kept under `paths.seed` so the NEXT attempt presents the SAME
// public key (NIM-865, docs/adr/0090-bootstrap-reply-loss-recovery.md).
//
// This is what makes a retry recognizable as a retry. The Keeper burns the
// bootstrap token when it SIGNS a certificate, which is one network hop short
// of the host holding one; if the reply is lost — a slow Vault inside the
// client's deadline is enough — the token is spent and only a presentation
// carrying the key that burn bound can complete the onboarding. Generating a
// fresh key per attempt, as this did before, made every retry look like a
// second host asking for a second identity, which is the one thing the
// one-time token exists to refuse.
//
// It deliberately sits BESIDE the version directories, not in one. [seed.Load]
// reads only through the `current` symlink, so a pending key is invisible to
// it and cannot be mistaken for a half-written seed — `ErrIncomplete` still
// means exactly what it meant.
//
// A private key with no certificate is not a credential: it authenticates
// nothing until the CA signs the matching public half, and the successful path
// writes these same bytes to the same directory at the same mode moments
// later. That is the trade the package header's "we leave nothing behind on
// disk" gives up, and it buys a host that can finish onboarding instead of one
// that is stranded until an operator issues a new token.
const PendingKeyFile = "pending-key.pem"

// loadOrCreatePendingKey returns the key this host onboards with, reusing the
// one a previous attempt left behind when there is one.
//
// A pending file that cannot be read or parsed is REPLACED rather than fatal.
// It can only have come from an attempt that already failed, the recovery it
// enables is an optimization over the first-presentation path, and refusing to
// onboard over a corrupt scratch file would turn a recoverable state into a
// permanently stuck one — the opposite of the point.
func loadOrCreatePendingKey(seedDir, sid string) (key *rsa.PrivateKey, csrPEM []byte, reused bool, err error) {
	path := filepath.Join(seedDir, PendingKeyFile)
	if key, err = readPendingKey(path, sid); err == nil {
		if csrPEM, err = createCSR(key, sid); err != nil {
			return nil, nil, false, err
		}
		return key, csrPEM, true, nil
	}

	if key, csrPEM, err = generateKeyAndCSR(sid); err != nil {
		return nil, nil, false, err
	}
	if err = writePendingKey(path, key, sid); err != nil {
		return nil, nil, false, err
	}
	return key, csrPEM, false, nil
}

// pendingKeySIDHeader binds the pending key to the SID it was generated for.
//
// Without it the key is reused by whatever host finds it, and the server side
// binds a public key to ONE SID for good — so a golden image snapshotted after
// a failed `soul init` would carry one key into every clone, the first clone
// would bind it, and every other clone would be refused forever with no way to
// tell why from the wire. The same wall catches a single host whose FQDN
// changes between attempts (DHCP or cloud-init settling after the first try).
// A SID mismatch is treated exactly like a corrupt file: generate a fresh key.
const pendingKeySIDHeader = "Soul-SID"

func readPendingKey(path, sid string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("bootstrap: pending key is not PEM")
	}
	if got := block.Headers[pendingKeySIDHeader]; got != sid {
		return nil, fmt.Errorf("bootstrap: pending key belongs to sid %q, not %q", got, sid)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: parse pending key: %w", err)
	}
	return key, nil
}

// writePendingKey lays the key down at the same mode [seed.Write] uses for the
// final one (0400 under a 0700 directory), via a temp file and a rename so a
// crash mid-write cannot leave a truncated key that the next attempt would
// parse as corrupt and discard.
//
// The temp file is written at 0400 in one call rather than created and
// chmod'ed, so a SIGKILL in between — a systemd TimeoutStartSec, a cloud-init
// step timeout, the very class of failure this file exists to survive — cannot
// leave a complete private key at a laxer mode.
//
// That mode is also why the sweep runs FIRST: the temp name is fixed, so a
// leftover 0400 file from an earlier kill would make this write fail with
// EACCES for a non-root `soul` and wedge the host permanently. Nothing else
// would ever collect those.
func writePendingKey(path string, key *rsa.PrivateKey, sid string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("bootstrap: mkdir %s: %w", dir, err)
	}
	sweepPendingKeyTemps(dir)

	tmpName := filepath.Join(dir, PendingKeyFile+".tmp")
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{pendingKeySIDHeader: sid},
		Bytes:   x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(tmpName, pemBytes, 0o400); err != nil {
		return fmt.Errorf("bootstrap: write pending key: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("bootstrap: install pending key: %w", err)
	}
	return nil
}

// sweepPendingKeyTemps removes half-written pending keys left by a process that
// died between the write and the rename. Best-effort by construction: this is
// cleanup, and failing an onboarding over it would be the wrong trade.
func sweepPendingKeyTemps(dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, PendingKeyFile+".*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// clearPendingKey removes the scratch key, and any half-written sibling, once
// the seed is on disk.
//
// Its error is deliberately NOT fatal to the onboarding — see the call site.
// By the time this runs the certificate is written and the token is spent, so
// reporting a failure here would tell the operator the bootstrap failed when it
// succeeded, and their natural response (re-run) would be a second
// presentation. An undeleted key is a stale file, not a broken host.
// It does NOT sweep temp files: a straggler can only exist between a killed
// write and the next one, and [writePendingKey] sweeps at that next write — a
// second call here could never fire on one, and a branch no test can reach is a
// branch that rots.
func clearPendingKey(seedDir string) error {
	err := os.Remove(filepath.Join(seedDir, PendingKeyFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
