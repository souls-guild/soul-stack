package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// testReleaseSource is the publication root the fixtures pretend their release came
// from. A descriptor must state one: it is what `plugin.allow` checks the operator's
// asserted `source` against before signing.
const testReleaseSource = "https://nexus.internal/plugins/redis"

// writeReleaseSlot creates an artifact-kind slot: `<root>/<alias>/<release_id>/` with a
// `<os>-<arch>/<alias>` file per row and the descriptor beside them, plus
// `current → <release_id>`. Returns the slot directory.
//
// The bodies differ per platform on purpose — a release is several sets of bytes, and a
// fixture where they were identical would hide a selection bug.
func writeReleaseSlot(t *testing.T, root, alias string, doc schema.Document, rows []ReleaseArtifact) string {
	t.Helper()
	pluginDir := filepath.Join(root, alias)

	filled := make([]ReleaseArtifact, len(rows))
	copy(filled, rows)
	staging := t.TempDir()
	for i, row := range filled {
		dir := filepath.Join(staging, row.Dir())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir platform dir: %v", err)
		}
		path := filepath.Join(dir, alias)
		stampedArtifact(t, path, doc, "#!/bin/sh\nexit 0\n# "+row.Dir()+"\n")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read artifact: %v", err)
		}
		sum := sha256.Sum256(raw)
		filled[i].SHA256 = hex.EncodeToString(sum[:])
	}

	release := Release{Kind: sharedplugin.SourceKindArtifact, Source: testReleaseSource, Artifacts: filled}
	if err := WriteRelease(staging, release); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
	releaseID, err := ReleaseID(release)
	if err != nil {
		t.Fatalf("ReleaseID: %v", err)
	}
	dir := filepath.Join(pluginDir, releaseID)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		t.Fatalf("promote staging: %v", err)
	}
	if err := os.Symlink(releaseID, filepath.Join(pluginDir, CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
	return dir
}

func twoPlatformRows() []ReleaseArtifact {
	return []ReleaseArtifact{
		{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64"},
		{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64"},
	}
}

// hostRows is a release covering the platform these tests run on, so the keeper-side
// discovery path has something to find.
func hostRows() []ReleaseArtifact {
	return []ReleaseArtifact{
		{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: "redis_here"},
		{OS: "plan9", Arch: "386", Path: "redis_elsewhere"},
	}
}

func TestReadSlot_ArtifactRelease(t *testing.T) {
	root := t.TempDir()
	rows := twoPlatformRows()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), rows)

	got, err := ReadSlot(root, "redis")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	if got.Kind != sharedplugin.SourceKindArtifact {
		t.Errorf("Kind = %q, want artifact", got.Kind)
	}
	if len(got.Artifacts) != 2 {
		t.Fatalf("Artifacts = %d, want 2", len(got.Artifacts))
	}
	for i, want := range rows {
		a := got.Artifacts[i]
		if a.OS != want.OS || a.Arch != want.Arch || a.Path != want.Path {
			t.Errorf("artifact %d = %+v, want os/arch/path of %+v", i, a, want)
		}
		if filepath.Base(filepath.Dir(a.BinaryPath)) != want.Dir() {
			t.Errorf("artifact %d lives at %q, want the %q platform directory", i, a.BinaryPath, want.Dir())
		}
	}
	// Each row's digest is RE-DERIVED from the file, not read out of the descriptor.
	for _, a := range got.Artifacts {
		raw, rerr := os.ReadFile(a.BinaryPath)
		if rerr != nil {
			t.Fatalf("read artifact: %v", rerr)
		}
		sum := sha256.Sum256(raw)
		if a.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("artifact %s/%s digest is not the file's", a.OS, a.Arch)
		}
	}
	// The disclosure is the one document the whole release carries.
	if got.Doc == nil || got.Doc.Kind != schema.KindSSHProvider {
		t.Errorf("Doc = %+v, want the release's cloud_driver document", got.Doc)
	}
	_ = dir
}

// A git slot has no descriptor, so it reads as a git slot. That is the whole
// compatibility story between the two layouts: the shape is what tells them apart, not
// a flag stored somewhere a migration would have to write.
func TestReadSlot_GitSlotStillReadsAsGit(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "hetzner", &doc, "hetzner")

	got, err := ReadSlot(root, "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	if got.Kind != sharedplugin.SourceKindGit || len(got.Artifacts) != 1 {
		t.Fatalf("git slot read as kind %q with %d artifacts", got.Kind, len(got.Artifacts))
	}
}

// The descriptor records what the resolver fetched; the bytes on disk are what will
// run. When they disagree the slot is refused, which is what keeps the descriptor from
// being a claim about digests nobody re-checked.
func TestReadSlot_TamperedArtifactIsFailClosed(t *testing.T) {
	root := t.TempDir()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), twoPlatformRows())

	victim := filepath.Join(dir, "linux-arm64", "redis")
	if err := os.WriteFile(victim, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := ReadSlot(root, "redis")
	if !errors.Is(err, ErrReleaseUnreadable) {
		t.Fatalf("err = %v, want ErrReleaseUnreadable", err)
	}
}

// A platform directory that is gone is the same class of failure: the release the
// descriptor describes is not what is on disk.
func TestReadSlot_MissingPlatformDirIsFailClosed(t *testing.T) {
	root := t.TempDir()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), twoPlatformRows())
	if err := os.RemoveAll(filepath.Join(dir, "linux-arm64")); err != nil {
		t.Fatalf("remove platform dir: %v", err)
	}
	if _, err := ReadSlot(root, "redis"); !errors.Is(err, ErrReleaseUnreadable) {
		t.Fatalf("err = %v, want ErrReleaseUnreadable", err)
	}
}

// One release, one disclosure. Platform builds that disclose different documents leave
// nothing a single signature could honestly approve, so the slot is refused rather than
// resolved to whichever document was read first.
func TestReadSlot_DivergentDisclosuresAreFailClosed(t *testing.T) {
	root := t.TempDir()
	rows := twoPlatformRows()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), rows)

	// A DIFFERENT disclosure for the same kind: the two platform builds must not be
	// able to say different things about what the plugin offers.
	other := schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "static_key",
	}
	victim := filepath.Join(dir, "linux-arm64", "redis")
	stampedArtifact(t, victim, other, "#!/bin/sh\nexit 0\n# linux-arm64\n")
	// Re-point the descriptor at the new digest so the ONLY remaining disagreement is
	// the disclosure — otherwise this would pass for the digest reason instead.
	rewriteReleaseDigest(t, dir, "linux-arm64", victim)

	_, err := ReadSlot(root, "redis")
	if err == nil || !strings.Contains(err.Error(), "disclosure") {
		t.Fatalf("err = %v, want a refusal naming the two disclosures", err)
	}
}

// rewriteReleaseDigest re-stamps the descriptor with the current digest of one
// platform's file. Only for the disclosure test, which has to isolate that failure
// from the digest one.
func rewriteReleaseDigest(t *testing.T, dir, platform, binPath string) {
	t.Helper()
	release, err := ReadRelease(dir)
	if err != nil || release == nil {
		t.Fatalf("ReadRelease: %v", err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	sum := sha256.Sum256(raw)
	for i := range release.Artifacts {
		if release.Artifacts[i].Dir() == platform {
			release.Artifacts[i].SHA256 = hex.EncodeToString(sum[:])
		}
	}
	if err := os.Remove(filepath.Join(dir, ReleaseFileName)); err != nil {
		t.Fatalf("remove descriptor: %v", err)
	}
	if err := WriteRelease(dir, *release); err != nil {
		t.Fatalf("WriteRelease: %v", err)
	}
}

// The release_id is derived from the descriptor, so the same release is the same
// directory whatever order the rows were written in — which is what makes a re-resolve
// a no-op instead of a rewrite.
func TestReleaseID_IsOrderIndependentAndContentAddressed(t *testing.T) {
	rows := []ReleaseArtifact{
		{OS: "linux", Arch: "amd64", Path: "a", SHA256: strings.Repeat("a", 64)},
		{OS: "linux", Arch: "arm64", Path: "b", SHA256: strings.Repeat("b", 64)},
	}
	forward, err := ReleaseID(Release{Kind: sharedplugin.SourceKindArtifact, Source: testReleaseSource, Artifacts: rows})
	if err != nil {
		t.Fatalf("ReleaseID: %v", err)
	}
	reversed, err := ReleaseID(Release{Kind: sharedplugin.SourceKindArtifact, Source: testReleaseSource,
		Artifacts: []ReleaseArtifact{rows[1], rows[0]}})
	if err != nil {
		t.Fatalf("ReleaseID reversed: %v", err)
	}
	if forward != reversed {
		t.Errorf("release_id depends on row order: %q != %q", forward, reversed)
	}

	changed := []ReleaseArtifact{rows[0], {OS: "linux", Arch: "arm64", Path: "b", SHA256: strings.Repeat("c", 64)}}
	other, err := ReleaseID(Release{Kind: sharedplugin.SourceKindArtifact, Source: testReleaseSource, Artifacts: changed})
	if err != nil {
		t.Fatalf("ReleaseID changed: %v", err)
	}
	if other == forward {
		t.Error("release_id unchanged after a digest changed — the slot would not be immutable")
	}

	// The recorded source is part of the slot's identity too: the same files published
	// at a second address are a different release, and must not share a directory with
	// the first — otherwise `plugin.allow`'s source check would compare against
	// whichever of the two happened to be resolved last.
	moved, err := ReleaseID(Release{Kind: sharedplugin.SourceKindArtifact,
		Source: "https://mirror.internal/plugins/redis", Artifacts: rows})
	if err != nil {
		t.Fatalf("ReleaseID moved: %v", err)
	}
	if moved == forward {
		t.Error("release_id unchanged after the source changed")
	}
}

// A descriptor that cannot be read as a release is refused rather than treated as an
// absent one: "there is nothing here" and "what is here is broken" are different facts,
// and only the second means somebody should look at the cache.
func TestReadRelease_Rejections(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{`,
		"unknown kind":   `{"kind":"torrent","artifacts":[{"os":"linux","arch":"amd64","path":"p","sha256":"` + strings.Repeat("a", 64) + `"}]}`,
		"empty list":     `{"kind":"artifact","artifacts":[]}`,
		"duplicate rows": `{"kind":"artifact","artifacts":[{"os":"linux","arch":"amd64","path":"a","sha256":"` + strings.Repeat("a", 64) + `"},{"os":"linux","arch":"amd64","path":"b","sha256":"` + strings.Repeat("b", 64) + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ReleaseFileName), []byte(body), 0o444); err != nil {
				t.Fatalf("write descriptor: %v", err)
			}
			if _, err := ReadRelease(dir); !errors.Is(err, ErrReleaseUnreadable) {
				t.Fatalf("err = %v, want ErrReleaseUnreadable", err)
			}
		})
	}

	// No descriptor at all is a git slot, not a failure.
	rel, err := ReadRelease(t.TempDir())
	if err != nil || rel != nil {
		t.Errorf("a slot with no descriptor: rel=%v err=%v, want (nil, nil)", rel, err)
	}
}

// Keeper discovery reads an artifact release through the platform directory for its
// OWN platform: a release holds one binary per platform, and the Keeper spawns what it
// discovers, so another platform's build is not something to fall back to.
func TestDiscover_ArtifactReleaseUsesTheHostPlatform(t *testing.T) {
	root := t.TempDir()
	writeReleaseSlot(t, root, "hetzner", sshProviderDoc(), hostRows())

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %d (warns %v), want the host's artifact", len(found), warns)
	}
	wantDir := runtime.GOOS + "-" + runtime.GOARCH
	if got := filepath.Base(filepath.Dir(found[0].BinaryPath)); got != wantDir {
		t.Errorf("discovered %q, want the %q platform directory", got, wantDir)
	}
}

// A release covering no platform this Keeper runs on is a warning and a skip, never a
// silent absence: the operator declared a plugin this Keeper cannot execute, and
// "not discovered" with nothing in the log reads exactly like "not declared".
func TestDiscover_ReleaseWithoutTheHostPlatformWarns(t *testing.T) {
	root := t.TempDir()
	writeReleaseSlot(t, root, "hetzner", sshProviderDoc(), []ReleaseArtifact{
		{OS: "plan9", Arch: "386", Path: "elsewhere"},
	})

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %d, want none", len(found))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], runtime.GOOS) {
		t.Fatalf("warns = %v, want one naming this platform", warns)
	}
}
