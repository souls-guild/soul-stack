package artifact

// THE INVARIANT, guarded where the snapshot DIRECTORY is assembled ([ADR-0085],
// NIM-728): `<cacheRoot>/<name>/<sha1>` is built from the service identifier, and
// a Service's `label` is one of the four surfaces the ADR names as off-limits.
//
// This package never sees a caption — [ServiceRef] carries the git coordinates
// and nothing else, which is the structural half of the guarantee and is asserted
// below. The behavioural half lives one layer out, at the only place a registry
// row becomes a Ref
// (keeper/internal/scenario/label_invariant_guard_test.go).
//
// Both halves are needed. The mapping test catches a substitution at the seam;
// this file catches a caption being introduced INTO the artifact layer, which
// would give the substitution somewhere to come from.
//
// HOW TO BREAK IT ON PURPOSE (the mutation this file exists to catch):
// add `Label string` to ServiceRef and build the layout from it — the reflection
// assertion goes red, and so does the path assertion if the layout follows.

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	guardArtifactServiceID    = "redis"
	guardArtifactServiceLabel = "redis-billing" // also a valid cache name — see below
)

// TestServiceRefCarriesNoCaption pins the shape of the address this package is
// handed. A caption cannot reach a snapshot directory without first appearing
// here, so this is the cheapest place to refuse it.
func TestServiceRefCarriesNoCaption(t *testing.T) {
	refT := reflect.TypeOf(ServiceRef{})
	for i := range refT.NumField() {
		if strings.EqualFold(refT.Field(i).Name, "label") {
			t.Errorf("ServiceRef gained a %q field.\n"+
				"ADR-0085: a Service's label participates in nothing derived, and the snapshot "+
				"directory <cacheRoot>/<name>/<sha1> is one of the four surfaces named. A Ref is "+
				"deliberately the identifier plus git coordinates — nothing mutable, because a "+
				"caption in the cache path re-clones the world on every label edit.",
				refT.Field(i).Name)
		}
	}
}

// TestSnapshotDirIsBuiltFromTheIdentifier walks the real layout and pins that the
// directory is the identifier, and that a caption — even one that would pass
// `reCacheName` perfectly well — appears nowhere in it.
func TestSnapshotDirIsBuiltFromTheIdentifier(t *testing.T) {
	// The caption is a VALID cache name: if a substitution happened, this
	// constructor would NOT reject it, and only the assertions below would notice.
	if !reCacheName.MatchString(guardArtifactServiceLabel) {
		t.Fatalf("fixture is wrong: the caption %q must itself be a valid cache name, "+
			"or a substitution would fail for the wrong reason", guardArtifactServiceLabel)
	}

	root := t.TempDir()
	layout, err := newCacheLayout(root, guardArtifactServiceID)
	if err != nil {
		t.Fatalf("newCacheLayout: %v", err)
	}

	const sha1 = "0123456789abcdef0123456789abcdef01234567"
	dir := layout.snapshotDir(sha1)

	want := filepath.Join(root, guardArtifactServiceID, sha1)
	if dir != want {
		t.Errorf("snapshotDir = %q, want %q (built from the IDENTIFIER)", dir, want)
	}
	if strings.Contains(dir, guardArtifactServiceLabel) {
		t.Errorf("the CAPTION %q reached the snapshot directory %q.\n"+
			"ADR-0085: a caption in this path moves every cached artifact on every label edit.",
			guardArtifactServiceLabel, dir)
	}
	// The working clone and the service root share the same segment; a caption in
	// any of them is the same defect.
	for name, got := range map[string]string{
		"serviceDir": layout.serviceDir(),
		"workDir":    layout.workDir(),
	} {
		if !strings.Contains(got, string(filepath.Separator)+guardArtifactServiceID) {
			t.Errorf("%s = %q is not built from the identifier %q", name, got, guardArtifactServiceID)
		}
		if strings.Contains(got, guardArtifactServiceLabel) {
			t.Errorf("%s = %q carries the CAPTION %q", name, got, guardArtifactServiceLabel)
		}
	}
}
