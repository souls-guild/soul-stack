package module

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// TaskError reasons for the install step (open catalog, naming-rules → Error
// codes; ride as a message prefix on the final failed event — precedent:
// errand_module_not_allowed).
const (
	reasonNotAllowed   = "module_not_allowed"
	reasonFetchFailed  = "module_fetch_failed"
	reasonVerifyFailed = "module_verify_failed"
)

// applyInstalled implements state `installed` (ADR-065(c,f,g)).
//
// Normative order, unchanged by NIM-377 and load-bearing: allow-check BEFORE fetch →
// sha256 idempotency → fetch by content address → full Sigil verify BEFORE
// materialization → atomic install into the catalog slot `<paths.modules>/<alias>/`.
//
// The task names the ALIAS the operator registered, not a namespace and name: the
// artifact carries no self-name, and the alias is what the grant, the slot and every
// address derived from it agree on.
func (m *Module) applyInstalled(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest) error {
	alias, err := util.StringParam(req.GetParams(), "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	pin, err := util.OptStringParam(req.GetParams(), "ref")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !reAlias.MatchString(alias) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: expected a registration alias, got %q", "name", alias))
	}
	if m.deps.ModulesRoot == "" {
		return util.SendFailed(stream, "paths.modules is not set in soul.yml - module cache has nowhere to materialize")
	}

	// (1) allow-check BEFORE a single network byte.
	var rec *sharedhost.SigilRecord
	if m.deps.Sigils != nil {
		rec = m.deps.Sigils.Get(alias)
	}
	if rec == nil {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: no active Sigil grant for %q (kind: soul_module); run `keeper.plugin.allow alias=%s source=<source> ref=<ref>`",
			reasonNotAllowed, alias, alias))
	}
	if pin != "" && rec.Ref != pin {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: active grant %q is on ref %q, task expects ref %q (pin check, ADR-065)",
			reasonNotAllowed, alias, rec.Ref, pin))
	}
	// The kind comes from the grant's schema bytes — the same bytes the signature
	// covers. Reading it from the artifact instead would mean trusting a file we have
	// not verified yet, and at this point we have not even fetched it.
	doc, diags := sharedplugin.ParseDocument(sharedplugin.SchemaFileName, rec.Schema)
	if doc == nil || diag.HasErrors(diags) || doc.Kind != sharedplugin.KindSoulModule {
		return util.SendFailed(stream, fmt.Sprintf(
			"%s: grant %q does not confirm kind: soul_module (grant schema is corrupt or a different kind)",
			reasonNotAllowed, alias))
	}

	// (2a) this host's row in the grant (NIM-793). A grant approves a RELEASE, and a
	// release is one binary per platform; the row for this platform is what says which
	// bytes are approved here. No row → fail-closed, and NOT as a digest mismatch:
	// nothing was approved to compare against, so the fix is to publish and re-approve
	// a release covering the platform.
	//
	// The row is selected ONCE, here, from the host's OWN Soulprint facts — and the
	// same row is what step (3) fetches and step (4) verifies against. Two independent
	// derivations of "this host's platform", one for the fetch and one inside verify,
	// would agree today and diverge the first time either changed; the symptom would be
	// digest_mismatch, which reads as tampering.
	approved, err := m.selectApproved(rec)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("%s: %s: %v", reasonNotAllowed, alias, err))
	}

	// The slot is named by the alias and holds exactly one executable; the artifact
	// has no name of its own, so the alias names the file too.
	slotDir := filepath.Join(m.deps.ModulesRoot, alias)
	binPath := filepath.Join(slotDir, alias)

	// (2b) idempotency: the installed artifact already matches the active grant.
	if diskSHA, exists := sha256OfFile(binPath); exists && strings.EqualFold(diskSHA, approved.SHA256) {
		return sendInstalled(stream, false, alias, rec, approved, binPath, nil)
	}

	// (3) fetch by content address — from the source the grant names, or from Keeper
	// over the current EventStream session. Both are legitimate transports and which
	// one this run used rides in the final event (see [Module.fetch] for the rule).
	fetched, err := m.fetch(stream.Context(), alias, rec, approved)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("%s: %s: %v", reasonFetchFailed, alias, err))
	}

	// (4) full Sigil verify BEFORE materialization: sha256 of the bytes ==
	// grant + signature over the source-keyed block + schema hash
	// (shared/pluginhost, ADR-065(f)).
	if err := sharedhost.VerifyArtifactBytes(fetched.data, rec, approved, m.deps.Anchors); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("%s: %s: %v", reasonVerifyFailed, alias, err))
	}

	// (5) atomic install into the slot: clear the previous artifact and its digest
	// sidecar → atomic rename.
	if err := installSlot(slotDir, binPath, fetched.data); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("install %s: %v", alias, err))
	}

	// (6) hot-register (ADR-065(d)) — only on an actual install.
	if m.deps.Rescan != nil {
		m.deps.Rescan()
	}

	return sendInstalled(stream, true, alias, rec, approved, binPath, fetched)
}

// fetchAll assembles the artifact bytes from the server-streaming PluginChunk response.
func fetchAll(ctx context.Context, fetcher Fetcher, alias, sha string) ([]byte, error) {
	stream, err := fetcher.FetchModule(ctx, &keeperv1.PluginFetchRequest{
		Alias:        alias,
		BinarySha256: sha,
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		chunk, rerr := stream.Recv()
		if errors.Is(rerr, io.EOF) {
			return buf.Bytes(), nil
		}
		if rerr != nil {
			return nil, rerr
		}
		buf.Write(chunk.GetData())
	}
}

// installSlot materializes the slot: one executable, written via atomic rename
// (util.AtomicWrite). No schema file is written — the schema travels inside the
// artifact's trailer, which is what discovery and `plugin.allow` both read.
//
// Two things are cleared first. The previous artifact's digest sidecar, because Spawn
// would otherwise fail-closed the freshly installed bytes against a stale digest (see
// shared/pluginhost verifySigilAndSeal). And any other executable left in the slot,
// because a slot holds exactly ONE — a second one from an earlier install under a
// different filename would make the slot ambiguous and discovery would refuse it
// rather than guess which is current.
func installSlot(slotDir, binPath string, binData []byte) error {
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(slotDir, sharedhost.DigestSidecarName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := removeForeignArtifacts(slotDir, filepath.Base(binPath)); err != nil {
		return err
	}
	return util.AtomicWrite(binPath, binData, 0o755)
}

// removeForeignArtifacts deletes every executable in the slot except keep. Dot-files
// are left alone: those are the sidecar and the temp files of atomic writes, neither
// of which discovery considers an artifact.
func removeForeignArtifacts(slotDir, keep string) error {
	entries, err := os.ReadDir(slotDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == keep || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(slotDir, name)
		st, serr := os.Stat(path)
		if serr != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return rerr
		}
	}
	return nil
}

// sha256OfFile returns the file's hex digest; exists=false on absence or any
// read error (an atomic-rename overwrite will fix an unreadable slot).
func sha256OfFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// sendInstalled reports the slot. fetched is nil when nothing was fetched (the
// idempotent no-op): the transport keys are then absent rather than carrying a
// transport that was not used this run.
func sendInstalled(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, alias string, rec *sharedhost.SigilRecord, approved *sharedhost.SigilArtifact, binPath string, fetched *fetchResult) error {
	out := map[string]any{
		"name":   alias,
		"source": rec.Source,
		"ref":    rec.Ref,
		// The digest of the artifact installed HERE, not of the release: a register
		// consumer comparing it against the local file needs this host's row.
		"sha256":    approved.SHA256,
		"path":      binPath,
		"installed": true,
		"changed":   changed,
	}
	if fetched != nil {
		out["fetch_via"] = fetched.via
		if fetched.url != "" {
			out["fetch_url"] = fetched.url
		}
		if len(fetched.warnings) > 0 {
			out["warnings"] = util.StringsToAny(fetched.warnings)
		}
	}
	return util.SendFinal(stream, changed, out)
}
