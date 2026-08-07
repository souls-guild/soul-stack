// Package plugingit — git resolver for Keeper-side plugin sources
// (ADR-026 Sigil, F-fetch approach, slice A1-S1).
//
// Before A1-S1 catalog `keeper.yml::plugins.{cloud_drivers,ssh_providers}` carried
// `source`/`ref`, but they were NOT used — binary reached cache slot outside
// Soul Stack. Resolver closes this: for each catalog entry Keeper itself
// git-resolves `source`+`ref` to a commit_sha slot, extracting the ALREADY-BUILT
// artifact from `dist/` (F-fetch — no compilation on Keeper).
//
// Cache layout (R-nested layout, A1-S1):
//
//	<cacheRoot>/
//	  <alias>/                         # the registration alias, address level 1
//	    current -> <commit_sha>        # symlink to active slot (atomic)
//	    <commit_sha>/                  # immutable slot (commit_sha unique)
//	      <alias>                      # the artifact; schema lives in its trailer
//
// # There is no name to look up (NIM-377)
//
// An artifact carries no self-name — no namespace, no name, no publisher, no filename
// convention. Two things follow, and both are load-bearing:
//
//   - in the checkout, `dist/` holds EXACTLY ONE executable and the resolver takes it.
//     Zero or several is a fail-closed error, never a first-match guess: picking one
//     would let a repository's directory listing decide which bytes get signed;
//   - the slot is named by the REGISTRATION ALIAS the operator chose in the catalog,
//     not by anything the repository says about itself. Two publishers of the same
//     subject therefore cannot collide, and a repository cannot claim an address.
//
// git-egress — HIGH security risk. Git operations via go-git (pure-Go,
// no system `git` fork): hooks NOT executed by design, `ext::` transport
// does not exist, submodules not recursive by default, `file://` locked by
// scheme-allowlist ([validateGitScheme]); clone/fetch — shallow (Depth=1)
// under context timeout. Hardening invariant details — in [git.go].
// Resolved binary NOT executed and NOT marked trusted — trust given separately
// via `plugin.allow` + Sigil (S3/S4/S6), resolver only populates cache.
package plugingit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// currentLink — name of symlink to active commit_sha slot inside <alias>/.
const currentLink = "current"

// Sentinel errors for resolve of one catalog entry. ResolveCatalog maps them to
// per-entry warnings (fail-closed: broken entry skipped, Keeper does not crash).
var (
	// ErrRefNotResolved — ResolveRevision(<ref>^{commit}) found no commit (ref
	// does not exist as tag, branch, or full hash).
	ErrRefNotResolved = errors.New("plugingit: ref does not resolve to a commit")
	// ErrSchemaUnreadable — the artifact carries no readable schema document: no
	// trailer, a malformed trailer, or a document that does not validate. The schema
	// is the disclosure an operator approves, so an artifact that cannot state what it
	// offers is refused here rather than cached and approved blind.
	ErrSchemaUnreadable = errors.New("plugingit: artifact carries no readable schema document")
	// ErrArtifactNotFound — `dist/` does not hold exactly one executable (none, or
	// several). F-fetch requires an ALREADY-built artifact, and with no filename
	// convention left (NIM-377) "exactly one" is the whole rule — several is
	// ambiguous and taking the first would let listing order pick the bytes.
	ErrArtifactNotFound = errors.New("plugingit: dist/ does not hold exactly one built artifact")
	// ErrAliasInvalid — the catalog's `name` is not a usable registration alias:
	// malformed ([sharedplugin.AliasPattern]) or on the closed reserved list
	// ([sharedplugin.IsReserved]). Fail-closed per entry — a reserved alias would
	// shadow an engine address such as `core.file.present`.
	ErrAliasInvalid = errors.New("plugingit: invalid registration alias")
	// ErrSourceUnavailable — git clone/fetch/checkout of source failed
	// (remote unavailable, auth, timeout).
	ErrSourceUnavailable = errors.New("plugingit: git source unavailable")
	// ErrArtifactTooLarge — the built artifact exceeds
	// plugins.max_artifact_size_mb. git-egress hardening (ADR-026(g)):
	// adversarial/huge artifact must not fill keeper-host cache. Fail-closed
	// — slot not created.
	ErrArtifactTooLarge = errors.New("plugingit: built artifact exceeds size limit")
	// ErrCloneTooLarge — total size of clone working tree (checkout + .git)
	// exceeds plugins.max_clone_size_mb. git-egress hardening (ADR-026(g)):
	// huge repository must not fill work_root. Fail-closed — workdir
	// cleaned, slot not created.
	ErrCloneTooLarge = errors.New("plugingit: clone tree exceeds size limit")
)

// DefaultGitTimeout — default timeout for resolver git-operation chain
// (clone/fetch → resolve → checkout). Matches config default
// [config.DefaultPluginFetchTimeout].
const DefaultGitTimeout = config.DefaultPluginFetchTimeout

// artifactSubdir — subdirectory of built artifact in plugin repository
// (F-fetch: binary already built in dist/, Keeper does not compile).
const artifactSubdir = "dist"

// ResolvedSlot — result of successful resolve of one catalog entry: where
// immutable slot was placed and how it is identified.
type ResolvedSlot struct {
	// Alias — the REGISTRATION alias, address level 1, taken from the catalog entry
	// (`name:`) and from nowhere else. The checkout has no say in it: the artifact
	// carries no self-name, so what the operator declared is the only identity the
	// slot can have.
	Alias string
	// Source — the git remote the artifact came from, as-is from the catalog. Together
	// with Ref this is the artifact's only SIGNED identity (ADR-026 as amended by
	// NIM-377).
	Source string
	// Ref — operator-asserted label from catalog (`ref:`), as-is.
	Ref string
	// CommitSHA — 40-hex commit to which ref resolved. Slot identifier
	// (immutable: one commit_sha → one slot directory).
	CommitSHA string
	// SlotDir — absolute path of immutable slot
	// `<cacheRoot>/<alias>/<commit_sha>/`.
	SlotDir string
	// BinaryPath — absolute path of the artifact inside the slot.
	BinaryPath string
	// BinarySHA256 — SHA-256 (hex, lowercase) of the artifact in the slot.
	BinarySHA256 string
	// SchemaBytes — the canonical schema document read from the artifact's TRAILER,
	// byte-exact. This is what an operator approves and what the Sigil signature
	// covers; it is read WITHOUT executing the artifact, which at this point is not
	// yet approved.
	SchemaBytes []byte
	// Doc — SchemaBytes parsed and validated. Never nil on a successful resolve.
	Doc *sharedplugin.Document
}

// Resolver — git resolver for plugin catalog. cacheRoot — root of slot cache;
// workRoot — root of working clones (STRICTLY outside cacheRoot, so .git and checkout
// not in cache directory read by Discover/ReadSlot).
//
// maxArtifactSize / maxCloneSize — size limits for git-egress hardening (ADR-026(g),
// bytes): ceiling of single extracted binary and total clone working tree.
// Protection of keeper-host disk from adversarial/huge repository (timeout
// bounds egress by time, these — by volume). Exceeded — fail-closed.
type Resolver struct {
	cacheRoot       string
	workRoot        string
	gitTimeout      time.Duration
	maxArtifactSize int64
	maxCloneSize    int64
	logger          *slog.Logger
}

// NewResolver constructs resolver. gitTimeout <= 0 → [DefaultGitTimeout].
// maxArtifactSize / maxCloneSize <= 0 → defaults [config.DefaultPluginMaxArtifactSizeMB]
// / [config.DefaultPluginMaxCloneSizeMB] (resolve symmetric to Resolved* config methods).
// logger nil → slog.Default(). Git operations — go-git (pure-Go, no
// system `git` fork).
func NewResolver(cacheRoot, workRoot string, gitTimeout time.Duration, maxArtifactSize, maxCloneSize int64, logger *slog.Logger) *Resolver {
	if gitTimeout <= 0 {
		gitTimeout = DefaultGitTimeout
	}
	if maxArtifactSize <= 0 {
		maxArtifactSize = int64(config.DefaultPluginMaxArtifactSizeMB) * bytesPerMiB
	}
	if maxCloneSize <= 0 {
		maxCloneSize = int64(config.DefaultPluginMaxCloneSizeMB) * bytesPerMiB
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		cacheRoot:       cacheRoot,
		workRoot:        workRoot,
		gitTimeout:      gitTimeout,
		maxArtifactSize: maxArtifactSize,
		maxCloneSize:    maxCloneSize,
		logger:          logger,
	}
}

// bytesPerMiB — multiplier MiB→bytes, local copy of [config.bytesPerMiB]
// (unexported in shared/config), to default limits in bytes without
// import workaround.
const bytesPerMiB = 1024 * 1024

// ResolveCatalog resolves entire catalog cloud_drivers + ssh_providers +
// soul_modules. Per-entry errors converted to warnings (fail-closed):
// broken entry skipped, Keeper does not crash. Returns (successfully
// resolved slots, warnings, fatal error). fatal — only what breaks
// resolve IN PRINCIPLE (e.g., unable to create workRoot); nil plugins →
// empty result.
func (r *Resolver) ResolveCatalog(ctx context.Context, plugins *config.KeeperPlugins) ([]ResolvedSlot, []string, error) {
	if plugins == nil {
		return nil, nil, nil
	}
	var (
		slots    []ResolvedSlot
		warnings []string
	)
	entries := make([]config.PluginCatalogEntry, 0,
		len(plugins.CloudDrivers)+len(plugins.SSHProviders)+len(plugins.SoulModules))
	entries = append(entries, plugins.CloudDrivers...)
	entries = append(entries, plugins.SSHProviders...)
	entries = append(entries, plugins.SoulModules...)

	for _, e := range entries {
		slot, err := r.ResolveEntry(ctx, e)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"plugin %q (source=%q ref=%q): %v", e.Name, e.Source, e.Ref, err))
			continue
		}
		slots = append(slots, slot)
	}
	return slots, warnings, nil
}

// ResolveEntry resolves one catalog entry to an immutable commit_sha slot.
// Flow (F-fetch — no compilation):
//
//  1. the catalog's `name` must be a usable registration alias — well-formed and not
//     reserved (→ ErrAliasInvalid). Checked FIRST, before a byte of git egress: a
//     name that cannot be registered is not worth cloning for;
//  2. validateGitScheme(source): allowlist https/ssh/scp (file:// — only under
//     env flag); disallowed scheme → ErrSourceUnavailable;
//  3. workdir := <workRoot>/<alias>/ (outside cacheRoot, mode 0700);
//     go-git shallow clone (or fetch if clone exists);
//  4. commit_sha := resolveRef(<ref>^{commit}) (40-hex guaranteed by
//     plumbing.Hash type; unresolvable ref → ErrRefNotResolved);
//  5. checkout detached-HEAD to commit_sha (go-git, hooks not executed);
//  6. the single executable in <workdir>/dist/ (none or several →
//     ErrArtifactNotFound);
//  7. the canonical schema document from that artifact's TRAILER, WITHOUT executing
//     it (missing/malformed/invalid → ErrSchemaUnreadable);
//  8. dst := <cacheRoot>/<alias>/<commit_sha>/; if already valid → skip
//     (commit_sha immutable);
//  9. staging directory on the same fs → copy(artifact)+fsync → atomic rename;
//  10. atomic update of the symlink <cacheRoot>/<alias>/current → <commit_sha>;
//  11. binary_sha256 := sha256(<dst>/<alias>).
//
// The slot holds the artifact and nothing else. The schema is not written beside it:
// it lives in the trailer, so a second copy could only ever disagree with the bytes
// that get hashed and signed.
func (r *Resolver) ResolveEntry(ctx context.Context, e config.PluginCatalogEntry) (ResolvedSlot, error) {
	ctx, cancel := context.WithTimeout(ctx, r.gitTimeout)
	defer cancel()

	// The catalog's `name` IS the registration alias (until NIM-437 builds the real
	// registry). Validate it before any egress.
	alias := e.Name
	if err := ValidateAlias(alias); err != nil {
		return ResolvedSlot{}, err
	}
	if e.Source == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty source", ErrSourceUnavailable)
	}
	if e.Ref == "" {
		return ResolvedSlot{}, fmt.Errorf("%w: empty ref", ErrRefNotResolved)
	}
	if err := validateGitScheme(e.Source); err != nil {
		return ResolvedSlot{}, err
	}

	workdir := filepath.Join(r.workRoot, alias)
	commitSHA, err := r.prepareCheckout(ctx, workdir, e.Source, e.Ref)
	if err != nil {
		return ResolvedSlot{}, err
	}

	// `dist/` holds exactly one executable and we take it. There is no filename to
	// look up and no first-match fallback: the published `schema.json` sits beside the
	// artifact and is skipped for not being executable, but a second BINARY is
	// genuinely ambiguous and stops the entry.
	distDir := filepath.Join(workdir, artifactSubdir)
	artifactPath, err := pluginhost.SingleArtifactIn(distDir)
	if err != nil {
		return ResolvedSlot{}, fmt.Errorf("%w: %s: %v", ErrArtifactNotFound, distDir, err)
	}
	ast, err := os.Stat(artifactPath)
	if err != nil {
		return ResolvedSlot{}, fmt.Errorf("plugingit: stat artifact %q: %w", artifactPath, err)
	}
	if !ast.Mode().IsRegular() {
		return ResolvedSlot{}, fmt.Errorf("%w: %s is not a regular file", ErrArtifactNotFound, artifactPath)
	}
	// git-egress hardening (ADR-026(g)): cut off a huge artifact BEFORE the copy into
	// the cache using os.Stat size (LimitReader in copyFileSync — defense in depth
	// below). Fail-closed: the slot is not materialized.
	if ast.Size() > r.maxArtifactSize {
		return ResolvedSlot{}, fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrArtifactTooLarge, artifactPath, ast.Size(), r.maxArtifactSize)
	}

	// Disclosure comes out of the artifact itself, read by seeking from its end. The
	// artifact is NOT executed: at this moment it is not approved, and running it to
	// ask what it does would defeat the approval it is waiting for.
	schemaBytes, doc, err := readArtifactSchema(artifactPath)
	if err != nil {
		return ResolvedSlot{}, err
	}

	pluginDir := filepath.Join(r.cacheRoot, alias)
	dst := filepath.Join(pluginDir, commitSHA)
	// The artifact takes the alias as its filename in the slot — it has no name of its
	// own, and the readers find it by "the one executable here" anyway.
	slotBinPath := filepath.Join(dst, alias)

	// commit_sha is immutable: if the slot is already valid — skip the extraction and
	// only refresh `current` and the digest of what is already there.
	if !r.slotValid(dst, alias) {
		if err := r.materializeSlot(pluginDir, dst, artifactPath, alias, r.maxArtifactSize); err != nil {
			return ResolvedSlot{}, err
		}
	}

	if err := updateCurrentSymlink(pluginDir, commitSHA); err != nil {
		return ResolvedSlot{}, err
	}

	digest, err := fileDigest(slotBinPath)
	if err != nil {
		return ResolvedSlot{}, err
	}

	return ResolvedSlot{
		Alias:        alias,
		Source:       e.Source,
		Ref:          e.Ref,
		CommitSHA:    commitSHA,
		SlotDir:      dst,
		BinaryPath:   slotBinPath,
		BinarySHA256: digest,
		SchemaBytes:  schemaBytes,
		Doc:          doc,
	}, nil
}

// ValidateAlias checks a catalog entry's `name` as a REGISTRATION ALIAS: well-formed
// ([sharedplugin.AliasPattern]) and not on the closed reserved list.
//
// Both halves matter for a different reason. The shape keeps an alias usable as a
// directory name and as address level 1 (no dots, no slashes, no uppercase). The
// reserved list keeps an operator from naming a plugin `core`, which would let
// `core.file.present` in a diff mean somebody's plugin instead of the engine — the
// reason the list exists at all now that an operator, not the artifact, picks the word.
func ValidateAlias(alias string) error {
	switch {
	case alias == "":
		return fmt.Errorf("%w: empty alias", ErrAliasInvalid)
	case !sharedplugin.ValidAlias(alias):
		return fmt.Errorf("%w: %q must match %s", ErrAliasInvalid, alias, sharedplugin.AliasPattern)
	case sharedplugin.IsReserved(alias):
		return fmt.Errorf("%w: %q is reserved (%s)", ErrAliasInvalid, alias,
			strings.Join(sharedplugin.ReservedNames(), ", "))
	}
	return nil
}

// readArtifactSchema reads and validates the canonical schema document stamped into an
// artifact, returning the byte-exact payload alongside the parsed form.
//
// Both come back because both are needed and they must not be re-derived from each
// other: the bytes are what gets hashed and signed, the parsed document is what the
// caller reasons about. Every failure — no trailer, malformed trailer, unparseable or
// invalid document — is one error, [ErrSchemaUnreadable], because they mean the same
// thing to an operator: this artifact does not state what it offers, so it cannot be
// approved.
func readArtifactSchema(path string) ([]byte, *sharedplugin.Document, error) {
	payload, err := schema.ReadTrailerFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrSchemaUnreadable, path, err)
	}
	doc, diags := sharedplugin.ParseDocument(path, payload)
	if derr := sharedplugin.FirstError(diags); derr != nil {
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrSchemaUnreadable, path, derr)
	}
	if doc == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrSchemaUnreadable, path)
	}
	return payload, doc, nil
}

// prepareCheckout prepares working clone at workdir on ref and returns
// resolved 40-hex commit_sha. Shallow-clones (Depth=1) if clone missing,
// otherwise shallow fetch exactly ref; resolves ref to commit; then
// checkout detached-HEAD to this commit. workdir created mode 0700
// (service-user-only).
//
// transport/auth/timeout failures on clone/fetch → ErrSourceUnavailable;
// unresolvable ref → ErrRefNotResolved (from [resolveRef]).
func (r *Resolver) prepareCheckout(ctx context.Context, workdir, source, ref string) (string, error) {
	if err := os.MkdirAll(r.workRoot, 0o700); err != nil {
		return "", fmt.Errorf("plugingit: mkdir work root %q: %w", r.workRoot, err)
	}

	auth, err := authFor(source)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	repo, err := openOrClone(ctx, workdir, source, auth)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if err := fetch(ctx, repo, auth); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	commitSHA, err := resolveRef(repo, ref)
	if err != nil {
		return "", err
	}
	if err := checkout(repo, commitSHA); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}

	// git-egress hardening (ADR-026(g)): go-git no byte-cap on clone, so
	// measure working tree (checkout + .git) AFTER extraction but BEFORE copy
	// artifact to cache. Shallow Depth=1 already cuts history; this walk catches
	// huge working tree (junk files / inflated artifact). Exceeded —
	// fail-closed: clean workdir, slot not created.
	size, err := dirSize(workdir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if size > r.maxCloneSize {
		_ = os.RemoveAll(workdir)
		return "", fmt.Errorf("%w: %s = %d bytes > limit %d bytes", ErrCloneTooLarge, workdir, size, r.maxCloneSize)
	}
	return commitSHA, nil
}

// dirSize sums size of regular files in root subtree (du-like, excluding
// directories/symlinks). Interrupts on first walk error — partial traverse
// would make limit-check unreliable.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("plugingit: measure clone tree %q: %w", root, err)
	}
	return total, nil
}

// slotValid — true if dst already holds the artifact under binName (commit_sha slots
// are immutable; a re-resolve of the same commit is a skip).
func (r *Resolver) slotValid(dst, binName string) bool {
	st, err := os.Stat(filepath.Join(dst, binName))
	return err == nil && st.Mode().IsRegular()
}

// materializeSlot extracts the artifact into the immutable slot dst atomically: build
// in a staging directory ON THE SAME fs as dst (rename is atomic only within one fs),
// fsync, then os.Rename(staging, dst). artifactMax is the byte-cap for the copy
// (ADR-026(g)): copy through a LimitReader, and on overflow the staging directory is
// cleaned and ErrArtifactTooLarge is returned (fail-closed).
//
// Only the artifact is copied. The schema is inside it — writing a second copy beside
// it would create a file that could disagree with the bytes that are hashed and signed,
// and a reader that fell back to it would be approving a different disclosure than the
// one it executes.
func (r *Resolver) materializeSlot(pluginDir, dst, artifactSrc, binName string, artifactMax int64) error {
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir plugin dir %q: %w", pluginDir, err)
	}
	staging := filepath.Join(pluginDir, ".staging-"+randSuffix())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("plugingit: mkdir staging %q: %w", staging, err)
	}
	cleanup := func() { _ = os.RemoveAll(staging) }

	if err := copyFileSync(artifactSrc, filepath.Join(staging, binName), 0o755, artifactMax); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(staging, dst); err != nil {
		cleanup()
		// Race of two resolvers on same commit_sha: winner already created dst —
		// not an error (slot immutable, content identical).
		if r.slotValid(dst, binName) {
			return nil
		}
		return fmt.Errorf("plugingit: atomic rename staging→slot %q: %w", dst, err)
	}
	return nil
}

// fileDigest calculates file SHA-256 streaming (plugin binaries — tens of MB).
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("plugingit: open binary for digest %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("plugingit: read binary for digest %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFileSync copies src→dst with given mode and fsync (slot must survive
// crash before current switches to it). maxBytes > 0 — byte-cap
// (ADR-026(g) git-egress hardening): copy via LimitReader(maxBytes+1) and
// exceeded returns ErrArtifactTooLarge fail-closed (dst stays in staging,
// caller cleans). maxBytes <= 0 — no limit (manifest).
func copyFileSync(src, dst string, mode os.FileMode, maxBytes int64) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("plugingit: open %q: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("plugingit: create %q: %w", dst, err)
	}
	var reader io.Reader = in
	if maxBytes > 0 {
		// +1 byte: if LimitReader gives exactly maxBytes+1 — source exceeds cap.
		reader = io.LimitReader(in, maxBytes+1)
	}
	written, err := io.Copy(out, reader)
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: copy %q→%q: %w", src, dst, err)
	}
	if maxBytes > 0 && written > maxBytes {
		_ = out.Close()
		return fmt.Errorf("%w: %s > limit %d bytes", ErrArtifactTooLarge, src, maxBytes)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("plugingit: fsync %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("plugingit: close %q: %w", dst, err)
	}
	// OpenFile with mode subject to umask — set mode explicitly.
	if err := os.Chmod(dst, mode); err != nil {
		return fmt.Errorf("plugingit: chmod %q: %w", dst, err)
	}
	return nil
}

// updateCurrentSymlink atomically swaps <pluginDir>/current → commitSHA:
// creates temp-symlink nearby and os.Rename-s it to current (rename of symlink
// atomic within directory). Target — relative (commitSHA), so slot
// movable together with pluginDir.
func updateCurrentSymlink(pluginDir, commitSHA string) error {
	tmp := filepath.Join(pluginDir, ".current-"+randSuffix())
	if err := os.Symlink(commitSHA, tmp); err != nil {
		return fmt.Errorf("plugingit: create temp symlink in %q: %w", pluginDir, err)
	}
	if err := os.Rename(tmp, filepath.Join(pluginDir, currentLink)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("plugingit: atomic swap current symlink in %q: %w", pluginDir, err)
	}
	return nil
}

// randSuffix — short random suffix for staging/temp names (avoid
// collisions of parallel resolvers). crypto/rand — not for crypto-strength,
// but so two processes don't pick same suffix.
func randSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// copyFileSync's callers own the path shapes: both the workdir segment and the slot
// directory are the alias, and [ValidateAlias] has already refused anything that could
// escape a path (dots, slashes, `..`) before a single directory is created. The old
// sanitizeSegment fallback is gone with it — a name that needs sanitizing is a name
// that must not be registered at all.
