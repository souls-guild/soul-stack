package trial

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestExamples_PinnedDestinies — a destiny that left this tree for a pinned commit
// (scripts/destiny-source.sh, NIM-1062) does not come back as a copy, and every corpus
// service names it at the tag its pin stands for.
//
// `make trial` places the pinned tree beside the services. It refuses a copy left under
// examples/destiny — a second source for one name, like the duplicate whose binary install
// ran without the systemd sandbox the public one had — but only when the trial runs; this
// guard holds the same line in `make test`. The ref is checked nowhere else: L0 resolves a
// destiny by name only, so a service declaring another ref would be tested against a tree
// it does not declare and pass.
func TestExamples_PinnedDestinies(t *testing.T) {
	root := requireExamples(t)

	aliases := destinySource(t, "aliases")
	if len(aliases) == 0 {
		t.Fatal("scripts/destiny-source.sh carries no destiny, so this guard holds nothing — remove it with the catalog")
	}
	tags := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		tag := destinySource(t, "tag", alias)
		if len(tag) != 1 {
			t.Fatalf("scripts/destiny-source.sh tag %s printed %q, want one tag", alias, tag)
		}
		tags[alias] = tag[0]

		_, err := os.Stat(filepath.Join(root, "destiny", alias))
		switch {
		case err == nil:
			t.Errorf("examples/destiny/%s exists beside the pinned %s@%s: two sources for one destiny, "+
				"and the in-tree one is what NIM-1062 removed", alias, alias, tag[0])
		case !errors.Is(err, fs.ErrNotExist):
			t.Fatalf("stat examples/destiny/%s: %v", alias, err)
		}
	}

	manifests, err := filepath.Glob(filepath.Join(root, "service", "*", "service.yml"))
	if err != nil {
		t.Fatalf("glob service manifests: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatalf("no service.yml under %s/service — the walk matched nothing", root)
	}
	var consumers int
	for _, path := range manifests {
		rel, _ := filepath.Rel(root, path)
		m, _, diags, err := config.LoadServiceManifest(path, config.ValidateOptions{})
		if err != nil || m == nil || diag.HasErrors(diags) {
			t.Errorf("examples/%s does not load: %v %s", rel, err, formatDiags(diags))
			continue
		}
		for _, dep := range m.Destiny {
			tag, pinned := tags[dep.Name]
			if !pinned {
				continue
			}
			consumers++
			if dep.Ref != tag {
				t.Errorf("examples/%s declares destiny %s at %s, but scripts/destiny-source.sh pins %s: "+
					"`make trial` would prove a tree this service does not declare", rel, dep.Name, dep.Ref, tag)
			}
		}
	}
	if consumers == 0 {
		t.Fatalf("no service under examples/service declares a pinned destiny (%s), so the ref half of "+
			"this guard checks nothing — drop that half deliberately rather than let it pass empty",
			strings.Join(aliases, ", "))
	}
}

// destinySource runs scripts/destiny-source.sh with a verb that reads the catalog only.
func destinySource(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command(filepath.FromSlash("../../../scripts/destiny-source.sh"), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("scripts/destiny-source.sh %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.Fields(string(out))
}
