package pluginhost

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// [SlotArtifactByDigest] — the delivery-path read (NIM-816).
//
// FetchModule is content-addressed: the caller names a sha256, exactly one file can
// answer to it, and that file is what gets streamed. Reaching it through [ReadSlot]
// meant re-deriving the digest of EVERY platform in the release, reading every trailer
// and validating every schema document, to hand back one file — work proportional to
// the size of the release rather than of the answer, on the rollout path where the
// whole fleet arrives at once.
//
// What must not change, and is pinned below: the file that is served is hashed from
// disk on this call and must equal the digest asked for. The approval path
// ([sigil.Service.Allow]) keeps ReadSlot and its whole-release re-derivation — a
// signature is over the whole list, so every row has to be checked there.

// readCharsOf returns this process's rchar counter: bytes returned by read(2),
// page-cache hits included. Linux-only.
func readCharsOf(t testing.TB) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io unavailable: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "rchar:")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if perr != nil {
			t.Fatalf("parse rchar %q: %v", line, perr)
		}
		return n
	}
	t.Fatal("/proc/self/io has no rchar line")
	return 0
}

// artifactBytesForReleaseScan — one platform's artifact, big enough that reading three
// of them is unmistakable next to reading one.
const artifactBytesForReleaseScan = 4 << 20

func threePlatformRows() []ReleaseArtifact {
	return []ReleaseArtifact{
		{OS: "linux", Arch: "amd64", Path: "redis_linux_amd64"},
		{OS: "linux", Arch: "arm64", Path: "redis_linux_arm64"},
		{OS: "darwin", Arch: "arm64", Path: "redis_darwin_arm64"},
	}
}

// readDescriptorRows returns the release's rows as written to disk, digests filled in.
func readDescriptorRows(t *testing.T, dir string) []ReleaseArtifact {
	t.Helper()
	release, err := ReadRelease(dir)
	if err != nil {
		t.Fatalf("ReadRelease: %v", err)
	}
	if release == nil {
		t.Fatal("fixture wrote no release descriptor")
	}
	return release.Artifacts
}

// Serving one file reads one file. The measurement is the ticket's claim: a 500-host
// rollout of a three-platform release used to hash 75 MiB per host to deliver 25 MiB.
func TestSlotArtifactByDigest_ReadsOnlyTheRequestedArtifact(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("read-byte accounting comes from /proc/self/io")
	}
	root := t.TempDir()
	dir := writeReleaseSlotPadded(t, root, "redis", sshProviderDoc(), threePlatformRows(), artifactBytesForReleaseScan)
	rows := readDescriptorRows(t, dir)
	if len(rows) != 3 {
		t.Fatalf("fixture rows = %d, want 3", len(rows))
	}

	want := rows[0]
	st, err := os.Stat(filepath.Join(dir, want.Dir(), "redis"))
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	size := st.Size()

	before := readCharsOf(t)
	path, err := SlotArtifactByDigest(root, "redis", want.SHA256)
	if err != nil {
		t.Fatalf("SlotArtifactByDigest: %v", err)
	}
	narrow := readCharsOf(t) - before

	if filepath.Base(filepath.Dir(path)) != want.Dir() {
		t.Fatalf("resolved %q, want the artifact of %s", path, want.Dir())
	}

	// The whole-release read, for the comparison the ticket asks to be shown.
	before = readCharsOf(t)
	if _, rerr := ReadSlot(root, "redis"); rerr != nil {
		t.Fatalf("ReadSlot: %v", rerr)
	}
	wide := readCharsOf(t) - before

	narrowPasses := float64(narrow) / float64(size)
	widePasses := float64(wide) / float64(size)
	t.Logf("artifact %d bytes × 3 platforms: by-digest %.2f passes, whole-release %.2f passes", size, narrowPasses, widePasses)

	if narrowPasses > 1.5 {
		t.Errorf("by-digest read %.2f artifacts' worth, want at most 1.5 — it serves one file", narrowPasses)
	}
	// The comparison is the point of the change, so it is asserted rather than only
	// logged: if the two ever converge, either the narrow read regressed into a full
	// scan or the fixture stopped covering several platforms.
	if widePasses < 2.5 {
		t.Errorf("whole-release read %.2f artifacts' worth over a 3-platform release, want ~3 — the fixture is not exercising the difference", widePasses)
	}
}

// Every platform of the release is reachable by its own digest, and each answer is the
// file that actually carries it. A read that returned "the first artifact" would pass a
// single-platform test and hand out the wrong bytes here.
func TestSlotArtifactByDigest_ResolvesEachPlatformToItsOwnFile(t *testing.T) {
	root := t.TempDir()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), threePlatformRows())
	// The path goes through `current`, exactly as [ReadSlot]'s does — the two reads
	// name the active slot the same way, so a caller cannot tell from the path which
	// one answered.
	active := filepath.Join(root, "redis", CurrentLink)
	for _, row := range readDescriptorRows(t, dir) {
		path, err := SlotArtifactByDigest(root, "redis", row.SHA256)
		if err != nil {
			t.Fatalf("SlotArtifactByDigest(%s): %v", row.Dir(), err)
		}
		want := filepath.Join(active, row.Dir(), "redis")
		if path != want {
			t.Errorf("digest of %s resolved to %q, want %q", row.Dir(), path, want)
		}
	}

	// And it agrees with ReadSlot row for row: the narrow read is the same answer
	// computed from fewer files, not a different notion of what the slot holds.
	slot, err := ReadSlot(root, "redis")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	for _, a := range slot.Artifacts {
		path, derr := SlotArtifactByDigest(root, "redis", a.SHA256)
		if derr != nil {
			t.Fatalf("SlotArtifactByDigest(%s-%s): %v", a.OS, a.Arch, derr)
		}
		if path != a.BinaryPath {
			t.Errorf("%s-%s: by-digest %q, ReadSlot %q", a.OS, a.Arch, path, a.BinaryPath)
		}
	}
}

// The guard on the file that IS served: its bytes are hashed on this call, and a file
// that no longer matches the digest asked for is refused. This is the check the narrow
// read must keep, and the mutation that breaks it — returning the descriptor's path
// without re-deriving the digest — is what this test exists to catch.
func TestSlotArtifactByDigest_TamperedArtifactIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), threePlatformRows())
	rows := readDescriptorRows(t, dir)

	target := filepath.Join(dir, rows[0].Dir(), "redis")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nmalicious\n"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := SlotArtifactByDigest(root, "redis", rows[0].SHA256)
	if !errors.Is(err, ErrReleaseUnreadable) {
		t.Fatalf("err = %v, want ErrReleaseUnreadable — the bytes on disk no longer hash to the approved digest", err)
	}
}

// A digest no row claims resolves to nothing. Fail-closed: the slot may well hold other
// approved bytes, and "close enough" is not an answer a content-addressed read gets to
// give.
func TestSlotArtifactByDigest_UnknownDigestIsNotFound(t *testing.T) {
	root := t.TempDir()
	writeReleaseSlot(t, root, "redis", sshProviderDoc(), threePlatformRows())

	_, err := SlotArtifactByDigest(root, "redis", strings.Repeat("ab", 32))
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v, want ErrSlotNotFound", err)
	}
}

// The descriptor says WHERE to look and is not believed about WHAT is there. A row
// whose recorded digest no longer matches its file — the descriptor drifting, or being
// edited — is refused rather than turned into a path.
func TestSlotArtifactByDigest_DescriptorIsNotAClaim(t *testing.T) {
	root := t.TempDir()
	dir := writeReleaseSlot(t, root, "redis", sshProviderDoc(), threePlatformRows())
	rows := readDescriptorRows(t, dir)

	// Swap two platforms' bodies: each file is now intact and each digest is still a
	// digest of something in this release — but neither sits where the descriptor
	// says it does.
	a := filepath.Join(dir, rows[0].Dir(), "redis")
	b := filepath.Join(dir, rows[1].Dir(), "redis")
	rawA, err := os.ReadFile(a)
	if err != nil {
		t.Fatalf("read %s: %v", a, err)
	}
	rawB, err := os.ReadFile(b)
	if err != nil {
		t.Fatalf("read %s: %v", b, err)
	}
	if err := os.WriteFile(a, rawB, 0o755); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if err := os.WriteFile(b, rawA, 0o755); err != nil {
		t.Fatalf("swap: %v", err)
	}

	if _, err := SlotArtifactByDigest(root, "redis", rows[0].SHA256); !errors.Is(err, ErrReleaseUnreadable) {
		t.Fatalf("err = %v, want ErrReleaseUnreadable — the file at the row's location does not carry the row's digest", err)
	}
}

// A missing slot, and a slot whose `current` symlink dangles, are both "there is
// nothing here" — the same fail-closed answer [ReadSlot] gives, from the same helper.
func TestSlotArtifactByDigest_MissingSlot(t *testing.T) {
	root := t.TempDir()
	if _, err := SlotArtifactByDigest(root, "absent", strings.Repeat("ab", 32)); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v, want ErrSlotNotFound", err)
	}

	pluginDir := filepath.Join(root, "redis")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("gone", filepath.Join(pluginDir, CurrentLink)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := SlotArtifactByDigest(root, "redis", strings.Repeat("ab", 32)); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("dangling current: err = %v, want ErrSlotNotFound", err)
	}
}

// A git slot has no descriptor and holds exactly one executable, so "the file with this
// digest" can only be that one — and it is hashed the same way. The pre-NIM-793 layout
// keeps working through the narrow read.
func TestSlotArtifactByDigest_GitSlot(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "redis", &doc, "redis")

	slot, err := ReadSlot(root, "redis")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	sha := slot.Artifacts[0].SHA256

	path, err := SlotArtifactByDigest(root, "redis", sha)
	if err != nil {
		t.Fatalf("SlotArtifactByDigest: %v", err)
	}
	if path != slot.Artifacts[0].BinaryPath {
		t.Errorf("resolved %q, want %q", path, slot.Artifacts[0].BinaryPath)
	}

	if _, err := SlotArtifactByDigest(root, "redis", strings.Repeat("ab", 32)); err == nil {
		t.Error("a digest the git slot's artifact does not carry must not resolve")
	}
}
