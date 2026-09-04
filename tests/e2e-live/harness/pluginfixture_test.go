package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
)

// TestFixturePluginMatchesTheArtifactModel — the assumptions the fixture builder
// (plugin.go) makes about what an artifact IS, checked with no containers, no
// `go build` and no stand.
//
// NIM-377 changed that model underneath this harness: it deleted the hand-written
// manifest.yaml the builder was reading and moved the plugin's disclosure into a
// trailer on the artifact itself. Nothing in the tree said so. It surfaced the
// next time somebody spent twenty minutes on `make e2e-live-gate`, at bring-up,
// where a deleted file in THIS repository was reported as "the stand didn't come
// up" on all nine gate tests (NIM-515) — the one direction the classifier must
// never be wrong in (NIM-406). This test is what that commit would have failed
// against in under a second.
//
// It reads the model through `sdk/schema`, the SDK a plugin author publishes
// with, instead of restating it. A restatement is a second definition that goes
// on agreeing with the old model after the product has left it — which is the
// shape of the bug, not a check for it.
func TestFixturePluginMatchesTheArtifactModel(t *testing.T) {
	dir := filepath.Join(repoRoot(t), redisPluginDir)

	// (1) The document the builder stamps into the fixture is published where the
	// builder looks for it. Absent, and BuildRedisPlugin dies before the
	// product is reached at all.
	document, err := os.ReadFile(filepath.Join(dir, schema.SchemaFileName))
	if err != nil {
		t.Fatalf("the fixture plugin publishes no %s: %v", schema.SchemaFileName, err)
	}

	// (2) manifest.yaml is gone and stays gone. This is the file NIM-377 deleted
	// and the builder used to read; one reappearing here would mean both models
	// are live at once, which is how the disagreement lasted a whole release.
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err == nil {
		t.Errorf("%s/manifest.yaml exists — NIM-377 replaced it with the generated %s, and the slot holds the artifact and nothing else (ADR-065(g))",
			redisPluginDir, schema.SchemaFileName)
	}

	// (3) Canonical, because the signature is over these exact bytes (ADR-026): a
	// reformatted copy is a different artifact, and keeper would reject it three
	// steps into a live run rather than here.
	if canonical, err := schema.IsCanonical(document); err != nil || !canonical {
		t.Fatalf("%s is not canonical (%v) — regenerate it, do not hand-edit", schema.SchemaFileName, err)
	}

	// (4) The trailer round-trips: what the builder appends is what a reader
	// seeking from the end gets back, byte for byte. Keeper reads it that way at
	// plugin.allow WITHOUT executing the artifact — it is not approved yet — so an
	// artifact whose trailer will not read has no disclosure and every reader
	// fails closed.
	stamped := schema.AppendTrailer([]byte("\x7fELF this stands in for the binary"), document)
	got, err := schema.ReadTrailerAt(bytes.NewReader(stamped), int64(len(stamped)))
	if err != nil {
		t.Fatalf("a trailer written by AppendTrailer is unreadable: %v", err)
	}
	if !bytes.Equal(got, document) {
		t.Fatalf("the trailer round-trip changed the document: %d bytes in, %d out", len(document), len(got))
	}

	// (5) The addresses the fixtures write resolve. The registry key is
	// `<alias>.<module>`, and since NIM-524 its two halves come from different
	// people: the alias is the operator's at registration, the module is the
	// author's, inside the artifact. The harness supplies the first; this is the
	// only place the second is checked, because everything that writes
	// `redis.<object>.<action>` is YAML.
	//
	// Since NIM-767 there are SEVEN of them, one per object, so the set is checked
	// in both directions: an object the artifact stopped serving would leave the
	// scenarios addressing nothing, and one it started serving without a row here
	// would go unexercised by the live suite.
	var doc schema.Document
	if err := json.Unmarshal(document, &doc); err != nil {
		t.Fatalf("the published %s does not decode as a schema.Document: %v", schema.SchemaFileName, err)
	}
	names := doc.ModuleNames()
	sort.Strings(names)
	want := append([]string(nil), redisObjects...)
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the artifact declares modules %v, this package says they are %v — the fixtures write `%s.<object>.<action>` and one of those two is wrong",
			names, want, RedisAlias)
	}
}
