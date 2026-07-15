package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

// Host-side delivery layout. Fixed, so the Soul side looks for the binary
// and plugins at the same path in pull/push (ADR-004, docs/keeper/push.md).
// Changing it is a PM decision (affects the Soul agent).
const (
	hostSoulDir    = "/var/lib/soul-stack/bin"
	hostModulesDir = "/var/lib/soul-stack/modules"
	hostSoulFile   = "soul"
	hostFileMode   = "0755"
)

// moduleNameRe restricts the module name to a safe alphabet. The name comes
// from the keeper config (not from Soul), but even a trusted source is
// better validated — fail-closed on any deviation (dots, slashes, `..`,
// quotes) so no injection is possible into the shell command on the host.
var moduleNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Deliverer lays out the soul binary and registered modules on a push host
// per the fixed layout (`/var/lib/soul-stack/{bin,modules}/`) with SHA-256
// dedup: if the file is already on the host and matches — skip; otherwise
// overwrite.
//
// Called by the dispatcher BEFORE the `soul apply` exec. Fail-closed: any
// delivery error aborts the run before the applier starts (no point running
// a stale binary).
type Deliverer interface {
	Deliver(ctx context.Context, session Session, spec SoulSpec) error
}

// SoulSpec — what to deliver to the host: the path to the local soul binary
// + plugins (`soul-mod-*`). Versions are pinned by git tag (ADR-007);
// resolving concrete files on the keeper side is a layer above (S3/runner).
type SoulSpec struct {
	// SoulBinaryPath — the absolute path to the local ./soul binary on the
	// keeper node. Delivered as `<hostSoulDir>/soul` with mode 0755.
	SoulBinaryPath string
	// Modules — what to deliver into `<hostModulesDir>/<Name>`. Order doesn't matter.
	Modules []ModuleSpec
}

// ModuleSpec — a single plugin (`soul-mod-*`).
type ModuleSpec struct {
	// Name — the file name on the host (no directory). Validated by moduleNameRe.
	Name string
	// Path — the absolute path to the local file.
	Path string
}

// ShaDeliverer — the default Deliverer implementation over Session.Run (ssh
// exec). Doesn't pull in a separate SFTP dependency: everything needed is
// `mkdir -p`, `sha256sum`, writing a file via `cat > path` with stdin,
// `chmod`. This is both simpler for in-process tests (a Session mock covers
// 100% of the surface) and doesn't add an external module for the feature.
//
// Semantics are idempotent: if the host already has a file with the same
// SHA-256 — skip upload (check only). This is the hot path on repeat runs.
type ShaDeliverer struct{}

// NewShaDeliverer — constructor for DI-explicitness (Deps.Deliverer). Returns
// a pointer to an empty struct so swapping in a new implementation is trivial.
func NewShaDeliverer() *ShaDeliverer { return &ShaDeliverer{} }

// Deliver checks each SoulSpec file against the host and ships over any that
// don't match. Steps: validate spec → mkdir -p {bin,modules} → for each file:
// local sha256 → remote sha256 (sha256sum) → skip if it matches, otherwise
// upload + chmod 0755.
func (d *ShaDeliverer) Deliver(ctx context.Context, session Session, spec SoulSpec) error {
	if session == nil {
		return errors.New("push/delivery: session is nil")
	}
	if spec.SoulBinaryPath == "" {
		return errors.New("push/delivery: SoulBinaryPath обязателен")
	}
	for _, m := range spec.Modules {
		if !moduleNameRe.MatchString(m.Name) {
			return fmt.Errorf("push/delivery: недопустимое имя модуля %q (ожидался [a-zA-Z0-9._-]+)", m.Name)
		}
		if m.Path == "" {
			return fmt.Errorf("push/delivery: пустой Path у модуля %q", m.Name)
		}
	}

	// A single mkdir -p creates both directories — saves a roundtrip.
	if _, err := session.Run(ctx, fmt.Sprintf("mkdir -p %s %s", hostSoulDir, hostModulesDir), nil); err != nil {
		return fmt.Errorf("push/delivery: mkdir %s %s: %w", hostSoulDir, hostModulesDir, err)
	}

	if err := d.deliverFile(ctx, session, spec.SoulBinaryPath, path.Join(hostSoulDir, hostSoulFile)); err != nil {
		return fmt.Errorf("push/delivery: soul-бинарь: %w", err)
	}
	for _, m := range spec.Modules {
		remote := path.Join(hostModulesDir, m.Name)
		if err := d.deliverFile(ctx, session, m.Path, remote); err != nil {
			return fmt.Errorf("push/delivery: модуль %q: %w", m.Name, err)
		}
	}
	return nil
}

// deliverFile — sha256 comparison of the local and remote file + upload on
// mismatch. SHA-256 is a crypto-strength/speed tradeoff: sufficient for
// artifact dedup, no collisions.
func (d *ShaDeliverer) deliverFile(ctx context.Context, session Session, localPath, remotePath string) error {
	localSum, err := fileSha256(localPath)
	if err != nil {
		return fmt.Errorf("локальный sha256 %s: %w", localPath, err)
	}
	remoteSum, err := remoteSha256(ctx, session, remotePath)
	if err != nil {
		return fmt.Errorf("удалённый sha256 %s: %w", remotePath, err)
	}
	if remoteSum == localSum {
		return nil
	}

	// Ships over stdin: `cat > path` has no size limit and leaves no file
	// in /tmp. Shell-escaping the path isn't needed — the path is tightly
	// controlled (hostSoulDir + validated moduleNameRe).
	data, err := os.ReadFile(localPath) //nolint:gosec // the path is our own keeper-side artifact
	if err != nil {
		return fmt.Errorf("чтение локального файла %s: %w", localPath, err)
	}
	// `set -e; cat > path; chmod 0755 path` in one command: if chmod fails,
	// the run fails too (without a separate roundtrip).
	cmd := fmt.Sprintf("set -e; cat > %s && chmod %s %s", remotePath, hostFileMode, remotePath)
	if _, err := session.Run(ctx, cmd, data); err != nil {
		return fmt.Errorf("upload %s: %w", remotePath, err)
	}
	// Post-verification: confirm the written file matches the sha sum
	// (protection against truncation/corruption in transport). Cheap
	// relative to the upload.
	got, err := remoteSha256(ctx, session, remotePath)
	if err != nil {
		return fmt.Errorf("проверка sha256 после upload %s: %w", remotePath, err)
	}
	if got != localSum {
		return fmt.Errorf("sha256 после upload %s не совпал: got %s, want %s", remotePath, got, localSum)
	}
	return nil
}

// fileSha256 computes the sha256 hash of a local file streaming (without
// fully loading large binaries into memory).
func fileSha256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // the path is our own keeper-side artifact
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// remoteSha256 requests a file's sha256 hash on the host. If the file
// doesn't exist — an empty string (== "will never match the local one" →
// upload). On any other error — return err (fail-closed: don't deliver
// "blindly" if sha256sum/permissions failed).
//
// Parsing: `sha256sum <path>` prints `<hex>  <path>\n`; we care about the
// first field.
func remoteSha256(ctx context.Context, session Session, p string) (string, error) {
	// `[ -f path ] && sha256sum path || echo MISSING` — but shell-escaping is
	// annoying; simpler to check with a separate command.
	// Single-quotes around %s — a shell-injection safeguard for a future
	// widening of the `m.Name` regex; the path is under our control for now.
	stdout, err := session.Run(ctx, fmt.Sprintf("test -f '%s' && sha256sum '%s' || echo MISSING", p, p), nil)
	if err != nil {
		// `||` guarantees exit 0; sshd returns non-nil only on channel/exec
		// problems — that's no longer "file missing", it's a transport failure.
		return "", fmt.Errorf("ssh exec sha256sum: %w", err)
	}
	out := strings.TrimSpace(stdout)
	if out == "" || out == "MISSING" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) < 1 {
		return "", fmt.Errorf("неожиданный вывод sha256sum: %q", out)
	}
	hexSum := fields[0]
	if len(hexSum) != sha256.Size*2 {
		return "", fmt.Errorf("неожиданный sha256 в выводе: %q", hexSum)
	}
	return hexSum, nil
}
