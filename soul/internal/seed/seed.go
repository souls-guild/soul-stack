// Package seed lays out versioned SoulSeed material on disk for a Soul host.
//
// SoulSeed consists of three required PEM files:
//
//	cert.pem  — client certificate issued by Vault PKI via Keeper Bootstrap.
//	key.pem   — private key for cert.pem (mode 0o400, owner = soul-service-user).
//	ca.pem    — CA chain to verify the Keeper's server certificate.
//
// and one optional trust-anchor file (ADR-026, slice S2b):
//
//	sigil_pubkey.pem — PEM (SPKI) of the Sigil signing ed25519 public key,
//	    received in BootstrapReply. Soul uses it to verify plugin grant
//	    (PluginSigil) signatures in pull mode without a new bootstrap (slice S6).
//	    OPTIONAL: an empty pubkey (Sigil not configured on Keeper) is valid —
//	    the file is not written, and its absence doesn't make a version incomplete.
//
// Layout under `paths.seed` (directory 0o700):
//
//	paths.seed/
//	  current -> vN        # relative symlink to the active version
//	  v1/  cert.pem key.pem ca.pem [sigil_pubkey.pem]
//	  v2/  cert.pem key.pem ca.pem [sigil_pubkey.pem]
//	  ...
//
// Writing a new version and switching the active one are atomic (see [Write]):
// the whole version is written to `vN+1/`, then the `current` symlink is
// atomically repointed to it. Before the swap, `current` still points at the
// previous version, so a failure at any earlier step leaves a valid old active
// version in place (crash-safety). sigil_pubkey.pem is written into the same
// version before the swap, so the trust-anchor switches atomically along with
// cert/key/ca and survives a restart. Reads go transparently through
// `current/` (see [Load]). File names are fixed, not configurable.
//
// Hard cut M1: the old flat format (cert/key/ca directly under `paths.seed`)
// is NOT supported. Missing `current` → [ErrIncomplete] (operator re-runs
// `soul init`); there is no auto-migration.
package seed

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// File names within a version. Change only in sync with docs/soul/identity.md.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
	CAFile   = "ca.pem"
	// SigilPubKeyFile — optional Sigil trust-anchor (ADR-026, S2b).
	// Missing file = Sigil disabled, a valid state.
	SigilPubKeyFile = "sigil_pubkey.pem"

	// currentLink — relative symlink to the active version within paths.seed.
	currentLink = "current"
	// versionPrefix — prefix for version directories (`v1`, `v2`, …).
	versionPrefix = "v"
)

// Material — in-memory SoulSeed contents.
type Material struct {
	// CertPEM — client cert issued by the Keeper.
	CertPEM []byte
	// KeyPEM — private key for CertPEM. Never leaves the host,
	// must not be logged.
	KeyPEM []byte
	// CAPEM — Keeper's CA chain (to verify the server cert).
	CAPEM []byte
	// SigilPubKeyPEM — optional Sigil trust-anchor (ADR-026, S2b): PEM (SPKI)
	// of the plugin-grant signing ed25519 public key. nil/empty = Sigil not
	// configured on the Keeper; then sigil_pubkey.pem isn't written, and its
	// absence doesn't make the version incomplete (plugin verify disabled).
	SigilPubKeyPEM []byte
}

// Paths — file paths for the active seed version (under `dir/current/`).
type Paths struct {
	Cert string
	Key  string
	CA   string
	// SigilPubKey — path to the optional Sigil trust-anchor. The file at this
	// path may not exist (Sigil disabled) — callers must check existence,
	// not assume presence.
	SigilPubKey string
}

// PathsIn returns file paths for the active version, under `dir/current/`.
// The `current` symlink is transparent to open(2); the tls config reads
// material through it, so a version swap changes the source without
// re-initializing paths.
func PathsIn(dir string) Paths {
	cur := filepath.Join(dir, currentLink)
	return Paths{
		Cert:        filepath.Join(cur, CertFile),
		Key:         filepath.Join(cur, KeyFile),
		CA:          filepath.Join(cur, CAFile),
		SigilPubKey: filepath.Join(cur, SigilPubKeyFile),
	}
}

// ErrIncomplete — Load on a directory with no active version (no `current`,
// or the active version is missing one of the three files). A runtime
// condition ("soul init hasn't run yet"), not an I/O failure.
var ErrIncomplete = errors.New("seed: bootstrap not completed (no active version under paths.seed/current)")

// ErrMismatched — cert↔key pair on disk doesn't match (e.g. a partial/
// corrupted rotation that bypassed our atomic swap). Distinct from
// ErrIncomplete: "material exists but cert and key don't form a valid pair",
// not "material is missing".
var ErrMismatched = errors.New("seed: cert.pem and key.pem do not form a valid pair")

// Load reads the active version from `dir/current/{cert,key,ca}.pem` plus
// the optional `sigil_pubkey.pem`.
//
// Returns [ErrIncomplete] wrapped if `current` is missing or the active
// version lacks one of the three REQUIRED files (cert/key/ca) — the caller
// prints a "run soul init" hint. A missing sigil_pubkey.pem is NOT an error
// (Sigil disabled): [Material.SigilPubKeyPEM] stays nil. After reading,
// cert+key are checked for consistency via [tls.X509KeyPair]; a mismatch →
// [ErrMismatched] (without leaking the key into the error text).
func Load(dir string) (*Material, error) {
	if dir == "" {
		return nil, errors.New("seed: paths.seed is empty")
	}
	if _, err := os.Lstat(filepath.Join(dir, currentLink)); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrIncomplete, filepath.Join(dir, currentLink))
		}
		return nil, fmt.Errorf("seed: stat %s: %w", filepath.Join(dir, currentLink), err)
	}
	p := PathsIn(dir)
	certPEM, err := readMember(p.Cert)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readMember(p.Key)
	if err != nil {
		return nil, err
	}
	caPEM, err := readMember(p.CA)
	if err != nil {
		return nil, err
	}
	// Optional Sigil trust-anchor: NotExist → nil (Sigil disabled), not
	// ErrIncomplete. Any other I/O error fails (file exists but unreadable).
	sigilPub, err := readOptionalMember(p.SigilPubKey)
	if err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		// tls.X509KeyPair's error text doesn't contain the key itself, but we
		// wrap in a static ErrMismatched (no %w on err) to be sure no details
		// leak into logs.
		return nil, ErrMismatched
	}
	return &Material{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caPEM, SigilPubKeyPEM: sigilPub}, nil
}

// readMember reads one required version file. NotExist → ErrIncomplete
// (version incomplete); any other I/O error is wrapped with the file name.
func readMember(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrIncomplete, path)
		}
		return nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	return data, nil
}

// readOptionalMember reads an optional version file. NotExist → (nil, nil):
// absence is valid (for sigil_pubkey.pem it means Sigil disabled), not a
// sign of an incomplete version. Any other I/O error is wrapped with the file name.
func readOptionalMember(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("seed: read %s: %w", path, err)
	}
	return data, nil
}

// Write lays out Material into a new version `dir/vN+1/` and atomically
// repoints the `dir/current` symlink to it.
//
// Steps:
//  1. validate the cert↔key pair via [tls.X509KeyPair] — fail-fast BEFORE any
//     write; a mismatched pair never hits disk (error text carries no key
//     material);
//  2. compute the next version vN+1 (no versions → v1);
//  3. write the three required files into `dir/vN+1/` (mode cert/ca = 0o644,
//     key = 0o400) plus the optional sigil_pubkey.pem (0o644) if
//     SigilPubKeyPEM is non-empty; fsync each file and fsync the version
//     directory itself (crash-safety);
//  4. atomic swap: temp symlink → os.Rename over `current`, fsync `dir`;
//  5. best-effort pruning of versions older than the previous one (keeps
//     current + 1).
//
// Before step 4, `current` still points at the previous version — a failure
// in steps 1-3 leaves a valid old active version in place. sigil_pubkey.pem
// lands in the same vN+1 version, so the trust-anchor switches atomically
// with cert/key/ca and survives a restart (S6 pull-mode verify without a new
// bootstrap).
func Write(dir string, m *Material) error {
	if dir == "" {
		return errors.New("seed: paths.seed is empty")
	}
	if m == nil {
		return errors.New("seed: material is nil")
	}
	// (a) Validate the pair before any write. Don't wrap the error with key contents.
	if _, err := tls.X509KeyPair(m.CertPEM, m.KeyPEM); err != nil {
		return ErrMismatched
	}
	// (b) Seed directory + compute the next version.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("seed: mkdir %s: %w", dir, err)
	}
	next, err := nextVersion(dir)
	if err != nil {
		return err
	}
	verName := versionPrefix + strconv.Itoa(next)
	verDir := filepath.Join(dir, verName)
	// (c) Write the whole version into vN+1.
	if err := os.MkdirAll(verDir, 0o700); err != nil {
		return fmt.Errorf("seed: mkdir %s: %w", verDir, err)
	}
	if err := atomicWrite(filepath.Join(verDir, CertFile), m.CertPEM, 0o644); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(verDir, KeyFile), m.KeyPEM, 0o400); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(verDir, CAFile), m.CAPEM, 0o644); err != nil {
		return err
	}
	// Optional Sigil trust-anchor — only if present (Sigil configured).
	// Empty → don't create the file; Load treats absence as "Sigil disabled".
	if len(m.SigilPubKeyPEM) > 0 {
		if err := atomicWrite(filepath.Join(verDir, SigilPubKeyFile), m.SigilPubKeyPEM, 0o644); err != nil {
			return err
		}
	}
	// (d) fsync the version directory — without it, renames of files inside
	// vN+1 might not hit disk before a crash, leaving the version incomplete (R1).
	if err := fsyncDir(verDir); err != nil {
		return err
	}
	// (e) Atomic swap of the current symlink -> verName (relative).
	if err := swapCurrent(dir, verName); err != nil {
		return err
	}
	// (f) Best-effort cleanup: an error here doesn't fail Write — the version is already active.
	pruneOldVersions(dir, next)
	return nil
}

// nextVersion returns max(existing vN) + 1; no versions → 1.
func nextVersion(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("seed: read dir %s: %w", dir, err)
	}
	max := 0
	for _, e := range ents {
		if n, ok := parseVersion(e.Name()); ok && n > max {
			max = n
		}
	}
	return max + 1, nil
}

// parseVersion parses a version directory name `vN` into N (N ≥ 1).
func parseVersion(name string) (int, bool) {
	if !strings.HasPrefix(name, versionPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(versionPrefix):])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// swapCurrent atomically repoints the `dir/current` symlink to target
// (a version name relative to dir). Creates a temp symlink alongside it and
// os.Renames it over current (atomic on POSIX), then fsyncs dir.
func swapCurrent(dir, target string) error {
	tmp, err := os.CreateTemp(dir, "."+currentLink+".tmp-*")
	if err != nil {
		return fmt.Errorf("seed: create temp symlink in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// CreateTemp made a regular file — we need a symlink in its place.
	_ = tmp.Close()
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("seed: prepare temp symlink %s: %w", tmpName, err)
	}
	if err := os.Symlink(target, tmpName); err != nil {
		return fmt.Errorf("seed: create temp symlink %s -> %s: %w", tmpName, target, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, currentLink)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("seed: swap %s -> %s: %w", currentLink, target, err)
	}
	// fsync the directory — commits the symlink rename itself (R1).
	if err := fsyncDir(dir); err != nil {
		return err
	}
	return nil
}

// pruneOldVersions removes versions vK with K < current-1: keeps the active
// version plus one previous. Best-effort — errors are ignored (called after
// a successful swap, the active version is already in place).
func pruneOldVersions(dir string, current int) {
	keepFrom := current - 1
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n, ok := parseVersion(e.Name())
		if !ok || n >= keepFrom {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// fsyncDir opens the directory and fsyncs it, committing metadata (entries
// created/renamed within it) to disk. Critical for version-dir + swap
// crash-safety (R1).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("seed: open dir %s for fsync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("seed: fsync dir %s: %w", dir, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("seed: close dir %s: %w", dir, err)
	}
	return nil
}

// atomicWrite writes data to a temp file next to path and renames it.
// rename is atomic on POSIX within one filesystem; the temp file never
// leaves the directory (path is fixed by the caller).
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("seed: create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// The temp file must be removed on any error below.
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: fsync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("seed: close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("seed: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
